package worker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.pedge.io/proto/rpclog"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	filesync "github.com/pachyderm/pachyderm/src/server/pkg/sync"
	ppsserver "github.com/pachyderm/pachyderm/src/server/pps"
)

type APIServer struct {
	sync.Mutex
	protorpclog.Logger
	pachClient   *client.APIClient
	etcdClient   *etcd.Client
	pipelineInfo *pps.PipelineInfo
}

func NewAPIServer(pachClient *client.APIClient, etcdClient *etcd.Client, pipelineInfo *pps.PipelineInfo) *APIServer {
	return &APIServer{
		Mutex:        sync.Mutex{},
		Logger:       protorpclog.NewLogger(""),
		pachClient:   pachClient,
		etcdClient:   etcdClient,
		pipelineInfo: pipelineInfo,
	}
}

func (a *APIServer) downloadData(ctx context.Context, data []*pfs.FileInfo) error {
	for i, datum := range data {
		input := a.pipelineInfo.Inputs[i]
		if err := filesync.Pull(ctx, a.pachClient, filepath.Join(client.PPSInputPrefix, input.Name), datum, input.Lazy); err != nil {
			return err
		}
	}
	return nil
}

func (a *APIServer) runUserCode(ctx context.Context) error {
	// Create output directory (currently /pfs/out)
	if err := os.MkdirAll(client.PPSOutputPath, 0666); err != nil {
		return err
	}

	// Run user's binary
	t := a.pipelineInfo.Transform
	cmd := exec.Command(t.Cmd[0], t.Cmd[1:]...)
	cmd.Stdin = strings.NewReader(strings.Join(t.Stdin, "\n") + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	success := true
	if err := cmd.Run(); err != nil {
		success = false
		// Get exit code from user binary, and compare with pipeline config's
		// allowed exit codes. If actual exit code is allowed, we're done.
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				for _, returnCode := range t.AcceptReturnCode {
					if int(returnCode) == status.ExitStatus() {
						success = true
					}
				}
			}
		}
		if !success {
			fmt.Fprintf(os.Stderr, "error from exec: %s\n", err.Error())
		}
	}
	return nil
}

func (a *APIServer) uploadOutput(ctx context.Context, tag string) error {
	// hashtree is not thread-safe--guard with 'lock'
	var lock sync.Mutex
	tree := hashtree.NewHashTree()

	// Upload all files in output directory
	var g errgroup.Group
	if err := filepath.Walk(client.PPSOutputPath, func(path string, info os.FileInfo, err error) error {
		g.Go(func() (retErr error) {
			// Don't upload root directory
			if path == client.PPSOutputPath {
				return nil
			}

			// Get 'path' relative to /pfs/out (so we don't put /pfs/out/xyz into
			// the object store)
			relPath, err := filepath.Rel(client.PPSOutputPath, path)
			if err != nil {
				return err
			}

			// Put directory. Even if the directory is empty, that may be useful to
			// users
			// TODO(msteffen) write a test pipeline that outputs an empty directory and
			// make sure it's preserved
			if info.IsDir() {
				lock.Lock()
				defer lock.Unlock()
				tree.PutDir(relPath)
				return nil
			}

			// 1) Open the file, and upload it to pfs with PutBlock.
			// 2) Take the block refs returned by PutBlock and put them in 'tree'
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() {
				if err := f.Close(); err != nil && retErr == nil {
					retErr = err
				}
			}()

			blockRefs, err := a.pachClient.PutBlock(pfs.Delimiter_NONE, f)
			if err != nil {
				return err
			}
			lock.Lock()
			defer lock.Unlock()
			return tree.PutFile(relPath, blockRefs.BlockRef)
		})
		return nil
	}); err != nil {
		return err
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// 3) Put 'tree' into object store; tag it with hash(inputs + transform)
	// This way we can skip these inputs in the next job (just fetch the blocks
	// that were output from this run)
	finTree, err := tree.Finish()
	if err != nil {
		return err
	}

	treeBytes, err := hashtree.Serialize(finTree)
	if err != nil {
		return err
	}

	if _, err := a.pachClient.PutObject(bytes.NewReader(treeBytes), tag); err != nil {
		return err
	}

	return nil
}

func (a *APIServer) Process(ctx context.Context, req *ProcessRequest) (resp *ProcessResponse, retErr error) {
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())
	// We cannot run more than one user process at once; otherwise they'd be
	// writing to the same output directory. Acquire lock to make sure only one
	// user process runs at a time.
	a.Lock()
	defer a.Unlock()

	// ppsserver sorts inputs by input name, so this is stable even if
	// a.pipelineInfo.Inputs are reordered by the user
	tag, err := ppsserver.HashDatum(req.Data, a.pipelineInfo)
	if err != nil {
		return nil, err
	}
	treebytes := &bytes.Buffer{}
	if err := a.pachClient.GetTag(tag, treebytes); err == nil {
		// We've already computed the output for these inputs. Return immediately
		return &ProcessResponse{
			Tag: &pfs.Tag{tag},
		}, nil
	}

	if err := a.downloadData(ctx, req.Data); err != nil {
		return nil, err
	}
	if err := a.runUserCode(ctx); err != nil {
		return nil, err
	}
	if err := a.uploadOutput(ctx, tag); err != nil {
		return nil, err
	}
	return &ProcessResponse{
		Tag: &pfs.Tag{tag},
	}, nil
}

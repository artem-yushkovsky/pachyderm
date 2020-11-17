package obj

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/pachyderm/pachyderm/src/client/pkg/tracing"

	minio "github.com/minio/minio-go/v6"
)

// Represents minio client instance for any s3 compatible server.
type minioClient struct {
	*minio.Client
	bucket string
}

// Creates a new minioClient structure and returns
func newMinioClient(endpoint, bucket, id, secret string, secure bool) (*minioClient, error) {
	mclient, err := minio.New(endpoint, id, secret, secure)
	if err != nil {
		return nil, err
	}
	c := &minioClient{
		bucket: bucket,
		Client: mclient,
	}
	c.TraceOn(os.Stdout)
	return c, nil
}

// Creates a new minioClient S3V2 structure and returns
func newMinioClientV2(endpoint, bucket, id, secret string, secure bool) (*minioClient, error) {
	mclient, err := minio.NewV2(endpoint, id, secret, secure)
	if err != nil {
		return nil, err
	}
	c := &minioClient{
		bucket: bucket,
		Client: mclient,
	}
	c.TraceOn(os.Stdout)
	return c, nil
}

// Represents minio writer structure with pipe and the error channel
type minioWriter struct {
	ctx     context.Context
	errChan chan error
	pipe    *io.PipeWriter
}

// Creates a new minio writer and a go routine to upload objects to minio server
func newMinioWriter(ctx context.Context, client *minioClient, name string) *minioWriter {
	reader, writer := io.Pipe()
	w := &minioWriter{
		ctx:     ctx,
		errChan: make(chan error),
		pipe:    writer,
	}
	go func() {
		opts := minio.PutObjectOptions{
			ContentType: "application/octet-stream",
			PartSize:    uint64(8 * 1024 * 1024),
		}
		fmt.Printf("newMinioWriter goroutine 1 with bucket: %v, name: %v, reader: %v, opts: %v\n", client.bucket, name, reader, opts)
		_, err := client.PutObject(client.bucket, name, reader, -1, opts)
		fmt.Printf("newMinioWriter goroutine 2\n")
		if err != nil {
			fmt.Printf("newMinioWriter goroutine 3\n")
			reader.CloseWithError(err)
		}
		fmt.Printf("newMinioWriter goroutine 4\n")
		w.errChan <- err
		fmt.Printf("newMinioWriter goroutine 5\n")
	}()
	return w
}

func (w *minioWriter) Write(p []byte) (retN int, retErr error) {
	fmt.Printf("minioWriter.Write 1\n")
	span, _ := tracing.AddSpanToAnyExisting(w.ctx, "/Minio.Writer/Write")
	fmt.Printf("minioWriter.Write 2\n")
	defer func() {
		fmt.Printf("minioWriter.Write defer\n")
		tracing.FinishAnySpan(span, "bytes", retN, "err", retErr)
	}()
	fmt.Printf("minioWriter.Write 3: %v\n", p)
	return w.pipe.Write(p)
}

// This will block till upload is done
func (w *minioWriter) Close() (retErr error) {
	fmt.Printf("minioWriter.Close 1\n")
	span, _ := tracing.AddSpanToAnyExisting(w.ctx, "/Minio.Writer/Close")
	fmt.Printf("minioWriter.Close 2\n")
	defer func() {
		fmt.Printf("minioWriter.Close defer, err: %v\n", retErr)
		tracing.FinishAnySpan(span, "err", retErr)
	}()
	fmt.Printf("minioWriter.Close 3\n")
	if err := w.pipe.Close(); err != nil {
		fmt.Printf("minioWriter.Close 4, err: %v\n", err)
		return err
	}
	fmt.Printf("minioWriter.Close 5\n")
	return <-w.errChan
}

func (c *minioClient) Writer(ctx context.Context, name string) (io.WriteCloser, error) {
	return newMinioWriter(ctx, c, name), nil
}

func (c *minioClient) Walk(_ context.Context, name string, fn func(name string) error) error {
	recursive := true // Recursively walk by default.

	doneCh := make(chan struct{})
	defer close(doneCh)
	for objInfo := range c.ListObjectsV2(c.bucket, name, recursive, doneCh) {
		if objInfo.Err != nil {
			return objInfo.Err
		}
		if err := fn(objInfo.Key); err != nil {
			return err
		}
	}
	return nil
}

// limitReadCloser implements a closer compatible wrapper
// for a size limited reader.
type limitReadCloser struct {
	io.Reader
	ctx  context.Context
	mObj *minio.Object
}

func (l *limitReadCloser) Close() (err error) {
	return l.mObj.Close()
}

func (l *limitReadCloser) Read(p []byte) (retN int, retErr error) {
	span, _ := tracing.AddSpanToAnyExisting(l.ctx, "/Minio.Reader/Read")
	defer func() {
		tracing.FinishAnySpan(span, "bytes", retN, "err", retErr)
	}()
	return l.Reader.Read(p)
}

func (c *minioClient) Reader(ctx context.Context, name string, offset uint64, size uint64) (io.ReadCloser, error) {
	obj, err := c.GetObject(c.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// Seek to an offset to fetch the new reader.
	_, err = obj.Seek(int64(offset), 0)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		return &limitReadCloser{
			Reader: io.LimitReader(obj, int64(size)),
			ctx:    ctx,
			mObj:   obj,
		}, nil
	}
	return obj, nil
}

func (c *minioClient) Delete(_ context.Context, name string) error {
	return c.RemoveObject(c.bucket, name)
}

func (c *minioClient) Exists(ctx context.Context, name string) bool {
	_, err := c.StatObject(c.bucket, name, minio.StatObjectOptions{})
	tracing.TagAnySpan(ctx, "err", err)
	return err == nil
}

func (c *minioClient) IsRetryable(err error) bool {
	// Minio client already implements retrying, no
	// need for a caller retry.
	return false
}

func (c *minioClient) IsIgnorable(err error) bool {
	return false
}

// Sentinel error response returned if err is not
// of type *minio.ErrorResponse.
var sentinelErrResp = minio.ErrorResponse{}

func (c *minioClient) IsNotExist(err error) bool {
	errResp := minio.ToErrorResponse(err)
	if errResp.Code == sentinelErrResp.Code {
		return false
	}
	// Treat both object not found and bucket not found as IsNotExist().
	return errResp.Code == "NoSuchKey" || errResp.Code == "NoSuchBucket"
}

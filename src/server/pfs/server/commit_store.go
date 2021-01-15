package server

import (
	"context"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/fileset"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/track"
)

type commitStore interface {
	AddFileset(ctx context.Context, commit *pfs.Commit, filesetID fileset.ID) error
	GetFileset(ctx context.Context, commit *pfs.Commit) (filesetID *fileset.ID, err error)
	SetFileset(ctx context.Context, commit *pfs.Commit, id fileset.ID) error
	DropFilesets(ctx context.Context, commit *pfs.Commit) error
	// Deleter() track.Deleter
}

type memCommitStore struct {
	s *fileset.Storage

	mu       sync.Mutex
	staging  map[string][]fileset.ID
	finished map[string]fileset.ID
}

func newMemCommitStore(s *fileset.Storage) *memCommitStore {
	return &memCommitStore{
		s:        s,
		staging:  make(map[string][]fileset.ID),
		finished: make(map[string]fileset.ID),
	}
}

func (s *memCommitStore) AddFileset(ctx context.Context, commit *pfs.Commit, filesetID fileset.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitKey(commit)
	if _, exists := s.finished[key]; exists {
		return errors.Errorf("commit is finished")
	}
	id, err := s.s.Clone(ctx, filesetID, track.NoTTL)
	if err != nil {
		return err
	}
	ids := s.staging[key]
	ids = append(ids, *id)
	s.staging[key] = ids
	return nil
}

func (s *memCommitStore) GetFileset(ctx context.Context, commit *pfs.Commit) (*fileset.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitKey(commit)
	if id, exists := s.finished[key]; exists {
		return s.s.Clone(ctx, id, defaultTTL)
	}
	// return nil, errors.Errorf("commit is not finished")
	return s.s.Compose(ctx, s.staging[key], defaultTTL)
}

func (s *memCommitStore) UpdateFileset(ctx context.Context, commit *pfs.Commit, fn func(fileset.ID) (*fileset.ID, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitKey(commit)
	x, exists := s.finished[key]
	if !exists {
		id, err := s.s.Compose(ctx, s.staging[key], defaultTTL)
		if err != nil {
			return err
		}
		x = *id
	}
	y, err := fn(x)
	if err != nil {
		return err
	}
	id, err := s.s.Clone(ctx, *y, track.NoTTL)
	if err != nil {
		return err
	}
	s.finished[key] = *id
	return s.s.Drop(ctx, x)
}

func (s *memCommitStore) DropFilesets(ctx context.Context, commit *pfs.Commit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := commitKey(commit)
	delete(s.finished, key)
	delete(s.staging, key)
	return nil
}

var _ commitStore = &postgresCommitStore{}

type postgresCommitStore struct {
	db *sqlx.DB
	s  *fileset.Storage
	tr track.Tracker
}

func newPostgresCommitStore(db *sqlx.DB, tr track.Tracker, s *fileset.Storage) *postgresCommitStore {
	return &postgresCommitStore{
		db: db,
		s:  s,
		tr: tr,
	}
}

func (cs *postgresCommitStore) AddFileset(ctx context.Context, commit *pfs.Commit, id fileset.ID) error {
	// clone to remove the ttl.
	id2, err := cs.s.Clone(ctx, id, track.NoTTL)
	if err != nil {
		return err
	}
	var num int
	if err := cs.db.GetContext(ctx, &num,
		`INSERT INTO pfs.commit_diffs (repo_name, commit_id, fileset_id)
		VALUES ($1, $2, $3)
		RETURNING num
	`, commit.Repo.Name, commit.ID, *id2); err != nil {
		return err
	}
	return nil
}

func (cs *postgresCommitStore) GetFileset(ctx context.Context, commit *pfs.Commit) (*fileset.ID, error) {
	id, err := cs.getTotal(ctx, commit)
	if err == nil {
		return cs.s.Clone(ctx, *id, defaultTTL)
	}
	ids, err := cs.getDiff(ctx, commit)
	if err != nil {
		return nil, err
	}
	return cs.s.Compose(ctx, ids, defaultTTL)
}

func (cs *postgresCommitStore) SetFileset(ctx context.Context, commit *pfs.Commit, id fileset.ID) error {
	_, err := cs.db.ExecContext(ctx,
		`INSERT INTO pfs.commit_totals (repo_name, commit_id, fileset_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO UPDATE
		SET fileset_id = $3
		WHERE repo_name = $1 AND commit_id = $2
		`, commit.Repo.Name, commit.ID, id)
	return err
}

func (cs *postgresCommitStore) DropFilesets(ctx context.Context, commit *pfs.Commit) error {
	// TODO: do something about the potential dangling references
	diffIDs, err := cs.getDiff(ctx, commit)
	if err != nil {
		return err
	}
	for _, id := range diffIDs {
		if err := cs.s.Drop(ctx, id); err != nil {
			return err
		}
	}
	if _, err := cs.db.ExecContext(ctx, `DELETE FROM pfs.commit_diffs WHERE repo_name = $1 AND commit_id = $2`); err != nil {
		return err
	}
	id, err := cs.getTotal(ctx, commit)
	if err != nil {
		return err
	}
	if err := cs.s.Drop(ctx, *id); err != nil {
		return err
	}
	if _, err := cs.db.ExecContext(ctx, `DELETE FROM pfs.commit_totals WHERE repo_name = $1 AND commit_id = $2`); err != nil {
		return err
	}
	return nil
}

func (cs *postgresCommitStore) getDiff(ctx context.Context, commit *pfs.Commit) ([]fileset.ID, error) {
	var ids []fileset.ID
	if err := cs.db.SelectContext(ctx, &ids,
		`SELECT fileset_id FROM pfs.commit_diffs
		WHERE commit_id = $1 AND repo_name = $2
		ORDER BY num
		`, commit.ID, commit.Repo.Name); err != nil {
		return nil, err
	}
	return ids, nil
}

func (cs *postgresCommitStore) getTotal(ctx context.Context, commit *pfs.Commit) (*fileset.ID, error) {
	var id fileset.ID
	if err := cs.db.GetContext(ctx, &id,
		`SELECT fileset_id FROM pfs.commit_totals
		WHERE commit_id = $1
		AND repo_name = $2
	`, commit.ID, commit.Repo.Name); err != nil {
		return nil, err
	}
	return &id, nil
}

// func (cs *postgresCommitStore) Deleter() track.Deleter {
// 	return commitDeleter{}
// }

// SetupPostgresCommitStoreV0 runs SQL to setup the commit store.
func SetupPostgresCommitStoreV0(ctx context.Context, tx *sqlx.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE pfs.commit_diffs (
			repo_name VARCHAR(250) NOT NULL,
			commit_id VARCHAR(64) NOT NULL,
			num SERIAL NOT NULL,
			fileset_id VARCHAR(64) NOT NULL,
			PRIMARY KEY(repo, commit_id, num)
		);

		CREATE TABLE pfs.commit_totals (
			repo_name VARCHAR(250) NOT NULL,
			commit_id VARCHAR(64) NOT NULL,
			fileset_id VARCHAR(64) NOT NULL,
			PRIMARY KEY(repo, commit_id)
		);
	`)
	return err
}

// func commitDiffTrackerID(commit *pfs.Commit, n int) string {
// 	return fmt.Sprintf("commit/diff/%s/%s/%d", commit.Repo.Name, commit.ID, n)
// }

// func commitTotalTrackerID(commit *pfs.Commit) string {
// 	return fmt.Sprintf("commit/total/%s")
// }

// var _ track.Deleter = &commitDeleter{}

// type postgresCommitDeleter struct {
// 	cs *postgresCommitStore
// }

// func (d postgresCommitDeleter) Delete(ctx context.Context, id string) error {
// 	const diffPrefix = "commit/diff"
// 	const totalPrefix = "commit/total"
// 	switch {
// 	case strings.HasPrefix(id, diffPrefix):
// 		return nil // TODO
// 	case strings.HasPrefix(id, totalPrefix):
// 		return nil // TODO
// 	default:
// 		return errors.Errorf("commit deleter cannot delete %v", id)
// 	}
// }

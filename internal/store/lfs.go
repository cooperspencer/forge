package store

import (
	"context"
	"database/sql"
	"errors"
)

// Git LFS object bookkeeping. The bytes live on disk (see forge/lfs.go); the
// database records which objects exist and which repositories may serve them.

// LFSObjectSize returns the size of an object that repoID may serve, or
// ErrNotFound if the repository has no such object.
func (s *Store) LFSObjectSize(ctx context.Context, repoID int64, oid string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT o.size FROM lfs_repo_objects ro JOIN lfs_objects o ON o.oid=ro.oid
		WHERE ro.repo_id=? AND ro.oid=?`, repoID, oid).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return n, err
}

// LFSObjectKnown reports whether the object's bytes are stored at all (for
// any repository).
func (s *Store) LFSObjectKnown(ctx context.Context, oid string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM lfs_objects WHERE oid=?`, oid).Scan(&one) == nil
}

// LinkLFSObject records a stored object and makes it servable by repoID.
func (s *Store) LinkLFSObject(ctx context.Context, repoID int64, oid string, size int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO lfs_objects(oid,size,created_at) VALUES (?,?,?)`, oid, size, t); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO lfs_repo_objects(repo_id,oid,created_at) VALUES (?,?,?)`, repoID, oid, t); err != nil {
		return err
	}
	return tx.Commit()
}

// CopyLFSLinks lets repository to serve the given objects of repository from
// (all of them if oids is nil): used for forks and merged pull requests.
func (s *Store) CopyLFSLinks(ctx context.Context, from, to int64, oids []string) (int, error) {
	if oids == nil {
		res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO lfs_repo_objects(repo_id,oid,created_at)
			SELECT ?, oid, ? FROM lfs_repo_objects WHERE repo_id=?`, to, now(), from)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	total := 0
	for _, oid := range oids {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO lfs_repo_objects(repo_id,oid,created_at)
			SELECT ?, oid, ? FROM lfs_repo_objects WHERE repo_id=? AND oid=?`, to, now(), from, oid)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, tx.Commit()
}

// LFSUsage is the number and total size of objects repoID can serve.
func (s *Store) LFSUsage(ctx context.Context, repoID int64) (count int, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(o.size),0) FROM lfs_repo_objects ro
		JOIN lfs_objects o ON o.oid=ro.oid WHERE ro.repo_id=?`, repoID).Scan(&count, &bytes)
	return
}

// UnreferencedLFSObjects removes and returns objects no repository links to
// any more, so their files can be deleted.
func (s *Store) UnreferencedLFSObjects(ctx context.Context) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT oid FROM lfs_objects WHERE oid NOT IN (SELECT oid FROM lfs_repo_objects)`)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var oid string
		if err := rows.Scan(&oid); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, oid)
	}
	rows.Close()
	for _, oid := range out {
		if _, err := tx.ExecContext(ctx, `DELETE FROM lfs_objects WHERE oid=?`, oid); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

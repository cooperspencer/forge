package store

import (
	"context"

	"time"
)

type Mirror struct {
	ID        int64
	RepoID    int64
	Direction string // pull | push
	URL       string
	Username  string
	Secret    string // encrypted
	SSHKey    string // encrypted
	SSHPub    string
	Interval  time.Duration
	RunCI     bool
	LastSync  time.Time
	LastTry   time.Time
	LastError string
	NextSync  time.Time
	CreatedAt time.Time
}

const mirrorCols = `id, repo_id, direction, url, username, secret, ssh_key, ssh_pub, interval, run_ci, last_sync, last_try, last_error, next_sync, created_at`

func scanMirror(sc interface{ Scan(...any) error }) (*Mirror, error) {
	var m Mirror
	var iv, ls, lt, ns, ca int64
	if err := sc.Scan(&m.ID, &m.RepoID, &m.Direction, &m.URL, &m.Username, &m.Secret, &m.SSHKey, &m.SSHPub, &iv, &m.RunCI, &ls, &lt, &m.LastError, &ns, &ca); err != nil {
		return nil, notFound(err)
	}
	m.Interval = time.Duration(iv) * time.Second
	m.LastSync, m.LastTry, m.CreatedAt = unixOrZero(ls), unixOrZero(lt), time.Unix(ca, 0)
	m.NextSync = unixOrZero(ns)
	return &m, nil
}

func unixOrZero(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Store) CreateMirror(ctx context.Context, m *Mirror) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO mirrors(repo_id,direction,url,username,secret,ssh_key,ssh_pub,interval,run_ci,next_sync,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, m.RepoID, m.Direction, m.URL, m.Username, m.Secret, m.SSHKey, m.SSHPub,
		int64(m.Interval/time.Second), b2i(m.RunCI), unixOf(m.NextSync), now())
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	return nil
}

// UpdateMirror saves the settings (not the sync state).
func (s *Store) UpdateMirror(ctx context.Context, m *Mirror) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mirrors SET url=?, username=?, secret=?, ssh_key=?, ssh_pub=?, interval=?, run_ci=?, next_sync=? WHERE id=?`,
		m.URL, m.Username, m.Secret, m.SSHKey, m.SSHPub, int64(m.Interval/time.Second), b2i(m.RunCI), unixOf(m.NextSync), m.ID)
	return err
}

func (s *Store) MirrorByID(ctx context.Context, id int64) (*Mirror, error) {
	return scanMirror(s.db.QueryRowContext(ctx, `SELECT `+mirrorCols+` FROM mirrors WHERE id=?`, id))
}

func (s *Store) queryMirrors(ctx context.Context, where string, args ...any) ([]*Mirror, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mirrorCols+` FROM mirrors WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Mirror
	for rows.Next() {
		m, err := scanMirror(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RepoMirrors lists a repository's mirrors, the pull mirror first.
func (s *Store) RepoMirrors(ctx context.Context, repoID int64) ([]*Mirror, error) {
	return s.queryMirrors(ctx, `repo_id=? ORDER BY direction='push', id`, repoID)
}

// PullMirror returns the repository's pull mirror, or ErrNotFound.
func (s *Store) PullMirror(ctx context.Context, repoID int64) (*Mirror, error) {
	ms, err := s.queryMirrors(ctx, `repo_id=? AND direction='pull'`, repoID)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, ErrNotFound
	}
	return ms[0], nil
}

// DueMirrors lists mirrors whose next sync is at or before t.
func (s *Store) DueMirrors(ctx context.Context, t time.Time) ([]*Mirror, error) {
	return s.queryMirrors(ctx, `next_sync > 0 AND next_sync <= ? ORDER BY next_sync`, t.Unix())
}

// MirrorResult records a sync attempt; ok updates last_sync.
func (s *Store) MirrorResult(ctx context.Context, id int64, at time.Time, syncErr string, next time.Time) error {
	q := `UPDATE mirrors SET last_try=?, last_error=?, next_sync=? WHERE id=?`
	args := []any{at.Unix(), syncErr, unixOf(next), id}
	if syncErr == "" {
		q = `UPDATE mirrors SET last_try=?, last_error=?, next_sync=?, last_sync=? WHERE id=?`
		args = []any{at.Unix(), syncErr, unixOf(next), at.Unix(), id}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// ScheduleMirrors sets the next sync of a repository's mirrors in direction.
func (s *Store) ScheduleMirrors(ctx context.Context, repoID int64, direction string, at time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE mirrors SET next_sync=? WHERE repo_id=? AND direction=?`, at.Unix(), repoID, direction)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) DeleteMirror(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM mirrors WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AllMirrors is used when re-encrypting with a new key.
func (s *Store) AllMirrors(ctx context.Context) ([]*Mirror, error) {
	return s.queryMirrors(ctx, `1=1`)
}

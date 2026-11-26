package store

import (
	"context"
	"database/sql"
	"time"
)

// PRRevision is one version of a pull request's head.
type PRRevision struct {
	N         int
	HeadSHA   string
	BaseSHA   string // merge base with the base branch when pushed
	Pusher    string
	CreatedAt time.Time
}

// AddPRRevision records a new head unless it equals the latest revision;
// it returns the revision number (0: nothing recorded).
func (s *Store) AddPRRevision(ctx context.Context, issueID int64, head, base string, pusherID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	var last string
	err = tx.QueryRowContext(ctx, `SELECT n, head_sha FROM pr_revisions WHERE issue_id=? ORDER BY n DESC LIMIT 1`, issueID).Scan(&n, &last)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	if last == head {
		return 0, nil
	}
	var pusher any
	if pusherID > 0 {
		pusher = pusherID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pr_revisions(issue_id,n,head_sha,base_sha,pusher_id,created_at) VALUES (?,?,?,?,?,?)`,
		issueID, n+1, head, base, pusher, now()); err != nil {
		return 0, err
	}
	return n + 1, tx.Commit()
}

// PRRevisions lists a pull request's revisions, oldest first.
func (s *Store) PRRevisions(ctx context.Context, issueID int64) ([]PRRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.n, r.head_sha, r.base_sha, COALESCE(u.name,''), r.created_at
		FROM pr_revisions r LEFT JOIN users u ON u.id=r.pusher_id WHERE r.issue_id=? ORDER BY r.n`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRRevision
	for rows.Next() {
		var r PRRevision
		var ts int64
		if err := rows.Scan(&r.N, &r.HeadSHA, &r.BaseSHA, &r.Pusher, &ts); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenPRsForBase lists open pull requests into repoID's branch (the PRs
// stacked on a PR whose head is that branch).
func (s *Store) OpenPRsForBase(ctx context.Context, repoID int64, branch string) ([]*Issue, error) {
	rows, err := s.db.QueryContext(ctx, issueSelect+` WHERE i.state='open' AND i.repo_id=? AND p.base_branch=? ORDER BY i.number`, repoID, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// SetPRBase retargets a pull request to another base branch.
func (s *Store) SetPRBase(ctx context.Context, issueID int64, branch string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pull_requests SET base_branch=? WHERE issue_id=?`, branch, issueID)
	return err
}

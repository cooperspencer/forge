package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type Issue struct {
	ID        int64
	RepoID    int64
	Number    int64
	AuthorID  int64
	Author    string
	Title     string
	Body      string
	State     string // open | closed | merged
	IsPR      bool
	Labels    []string
	Comments  int
	CreatedAt time.Time
	// BoardColumn is the board column id (0 = none yet) and BoardPos the
	// position inside it; see Forge.Board.
	BoardColumn int64
	BoardPos    int
	UpdatedAt   time.Time

	PR *PullRequest // populated for pull requests
}

type PullRequest struct {
	HeadRepoID int64
	HeadRepo   string // owner/name
	HeadBranch string
	BaseBranch string
	HeadSHA    string
	MergeSHA   string
	MergedBy   string
}

const issueSelect = `SELECT i.id, i.repo_id, i.number, i.author_id, u.name, i.title, i.body, i.state, i.is_pr, i.labels,
	(SELECT COUNT(*) FROM comments c WHERE c.issue_id=i.id), i.created_at, i.updated_at,
	COALESCE(p.head_repo_id,0), COALESCE(hu.name || '/' || hr.name,''), COALESCE(p.head_branch,''), COALESCE(p.base_branch,''),
	COALESCE(p.head_sha,''), COALESCE(p.merge_sha,''), COALESCE(mu.name,''), i.board_column, i.board_pos
	FROM issues i JOIN users u ON u.id=i.author_id
	LEFT JOIN pull_requests p ON p.issue_id=i.id
	LEFT JOIN repos hr ON hr.id=p.head_repo_id LEFT JOIN users hu ON hu.id=hr.owner_id
	LEFT JOIN users mu ON mu.id=p.merged_by`

func scanIssue(sc interface{ Scan(...any) error }) (*Issue, error) {
	var i Issue
	var labels string
	var c, u int64
	var pr PullRequest
	if err := sc.Scan(&i.ID, &i.RepoID, &i.Number, &i.AuthorID, &i.Author, &i.Title, &i.Body, &i.State, &i.IsPR, &labels,
		&i.Comments, &c, &u, &pr.HeadRepoID, &pr.HeadRepo, &pr.HeadBranch, &pr.BaseBranch, &pr.HeadSHA, &pr.MergeSHA, &pr.MergedBy, &i.BoardColumn, &i.BoardPos); err != nil {
		return nil, notFound(err)
	}
	if labels != "" {
		i.Labels = strings.Split(labels, ",")
	}
	i.CreatedAt, i.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	if i.IsPR {
		i.PR = &pr
	}
	return &i, nil
}

func nextNumber(ctx context.Context, tx *sql.Tx, repoID int64) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM issues WHERE repo_id=?`, repoID).Scan(&n)
	return n, err
}

func (s *Store) CreateIssue(ctx context.Context, repoID, authorID int64, title, body string) (*Issue, error) {
	return s.createIssue(ctx, repoID, authorID, title, body, nil)
}

func (s *Store) CreatePullRequest(ctx context.Context, repoID, authorID int64, title, body string, pr PullRequest) (*Issue, error) {
	return s.createIssue(ctx, repoID, authorID, title, body, &pr)
}

func (s *Store) createIssue(ctx context.Context, repoID, authorID int64, title, body string, pr *PullRequest) (*Issue, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	n, err := nextNumber(ctx, tx, repoID)
	if err != nil {
		return nil, err
	}
	t := now()
	res, err := tx.ExecContext(ctx, `INSERT INTO issues(repo_id,number,author_id,title,body,is_pr,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		repoID, n, authorID, title, body, b2i(pr != nil), t, t)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if pr != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pull_requests(issue_id,head_repo_id,head_branch,base_branch,head_sha) VALUES (?,?,?,?,?)`,
			id, pr.HeadRepoID, pr.HeadBranch, pr.BaseBranch, pr.HeadSHA); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.IssueByNumber(ctx, repoID, n)
}

func (s *Store) IssueByNumber(ctx context.Context, repoID, number int64) (*Issue, error) {
	return scanIssue(s.db.QueryRowContext(ctx, issueSelect+` WHERE i.repo_id=? AND i.number=?`, repoID, number))
}

type IssueFilter struct {
	PRs   bool   // list pull requests instead of issues
	State string // "" = all, open, closed (closed includes merged)
	Query string
}

func (s *Store) ListIssues(ctx context.Context, repoID int64, f IssueFilter) ([]*Issue, error) {
	q := issueSelect + ` WHERE i.repo_id=? AND i.is_pr=?`
	args := []any{repoID, b2i(f.PRs)}
	switch f.State {
	case "open":
		q += ` AND i.state='open'`
	case "closed":
		q += ` AND i.state IN ('closed','merged')`
	}
	if f.Query != "" {
		q += ` AND (i.title LIKE ? OR i.body LIKE ?)`
		like := "%" + f.Query + "%"
		args = append(args, like, like)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY i.number DESC`, args...)
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

// OpenPRsForBranch finds open PRs whose head is repoID:branch (used to update head SHA on push).
func (s *Store) OpenPRsForHead(ctx context.Context, headRepoID int64, branch string) ([]*Issue, error) {
	rows, err := s.db.QueryContext(ctx, issueSelect+` WHERE i.state='open' AND p.head_repo_id=? AND p.head_branch=?`, headRepoID, branch)
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

func (s *Store) UpdateIssue(ctx context.Context, i *Issue) error {
	_, err := s.db.ExecContext(ctx, `UPDATE issues SET title=?, body=?, state=?, labels=?, updated_at=? WHERE id=?`,
		i.Title, i.Body, i.State, strings.Join(i.Labels, ","), now(), i.ID)
	return err
}

func (s *Store) SetPRHead(ctx context.Context, issueID int64, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pull_requests SET head_sha=? WHERE issue_id=?`, sha, issueID)
	if err == nil {
		s.db.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, now(), issueID)
	}
	return err
}

func (s *Store) MarkMerged(ctx context.Context, issueID, userID int64, mergeSHA string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE pull_requests SET merge_sha=?, merged_by=? WHERE issue_id=?`, mergeSHA, userID, issueID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET state='merged', updated_at=? WHERE id=?`, now(), issueID); err != nil {
		return err
	}
	return tx.Commit()
}

// ------------------------------------------------------------------ comments

type Comment struct {
	ID        int64
	IssueID   int64
	AuthorID  int64
	Author    string
	Body      string
	Review    string // '' | approve | changes | comment
	Path      string
	Line      int
	CreatedAt time.Time
}

func (s *Store) AddComment(ctx context.Context, c *Comment) error {
	if strings.TrimSpace(c.Body) == "" && c.Review == "" {
		return fmt.Errorf("comment is empty")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO comments(issue_id,author_id,body,review,path,line,created_at) VALUES (?,?,?,?,?,?,?)`,
		c.IssueID, c.AuthorID, c.Body, c.Review, c.Path, c.Line, now())
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	s.db.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, now(), c.IssueID)
	return nil
}

func (s *Store) Comments(ctx context.Context, issueID int64) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.issue_id, c.author_id, u.name, c.body, c.review, c.path, c.line, c.created_at
		FROM comments c JOIN users u ON u.id=c.author_id WHERE c.issue_id=? ORDER BY c.id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		var ts int64
		if err := rows.Scan(&c.ID, &c.IssueID, &c.AuthorID, &c.Author, &c.Body, &c.Review, &c.Path, &c.Line, &ts); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ releases

type Release struct {
	ID        int64
	RepoID    int64
	Tag       string
	Title     string
	Notes     string
	Author    string
	CreatedAt time.Time
}

func (s *Store) CreateRelease(ctx context.Context, repoID, authorID int64, tag, title, notes string) (*Release, error) {
	if title == "" {
		title = tag
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases(repo_id,tag,title,notes,author_id,created_at) VALUES (?,?,?,?,?,?)`,
		repoID, tag, title, notes, authorID, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("a release for %s already exists", tag)
		}
		return nil, err
	}
	rs, err := s.Releases(ctx, repoID)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Tag == tag {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) Releases(ctx context.Context, repoID int64) ([]*Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.repo_id, r.tag, r.title, r.notes, u.name, r.created_at
		FROM releases r JOIN users u ON u.id=r.author_id WHERE r.repo_id=? ORDER BY r.id DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		var r Release
		var ts int64
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Tag, &r.Title, &r.Notes, &r.Author, &ts); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(ts, 0)
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRelease(ctx context.Context, repoID int64, tag string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE repo_id=? AND tag=?`, repoID, tag)
	return err
}

// ------------------------------------------------------------------ events

type Event struct {
	ID        int64
	RepoID    int64
	Repo      string
	Actor     string
	Kind      string
	Summary   string
	CreatedAt time.Time
	Read      bool
}

// RecordEvent stores an activity entry and fans out notifications to watchers
// (excluding the actor).
func (s *Store) RecordEvent(ctx context.Context, repoID, actorID int64, kind, summary string) error {
	var actor any
	if actorID != 0 {
		actor = actorID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events(repo_id,actor_id,kind,summary,created_at) VALUES (?,?,?,?,?)`, repoID, actor, kind, summary, now())
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	_, err = s.db.ExecContext(ctx, `INSERT INTO notifications(user_id,event_id) SELECT user_id, ? FROM watches WHERE repo_id=? AND user_id<>?`, id, repoID, actorID)
	return err
}

const eventSelect = `SELECT e.id, COALESCE(e.repo_id,0), COALESCE(ou.name || '/' || r.name,''), COALESCE(a.name,'system'), e.kind, e.summary, e.created_at
	FROM events e LEFT JOIN repos r ON r.id=e.repo_id LEFT JOIN users ou ON ou.id=r.owner_id LEFT JOIN users a ON a.id=e.actor_id`

func scanEvents(rows *sql.Rows, withRead bool) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		dst := []any{&e.ID, &e.RepoID, &e.Repo, &e.Actor, &e.Kind, &e.Summary, &ts}
		if withRead {
			dst = append(dst, &e.Read)
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) RepoEvents(ctx context.Context, repoID int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, eventSelect+` WHERE e.repo_id=? ORDER BY e.id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows, false)
}

// Feed returns recent events from repositories visible to u.
func (s *Store) Feed(ctx context.Context, u *User, limit int) ([]Event, error) {
	repos, err := s.VisibleRepos(ctx, u)
	if err != nil || len(repos) == 0 {
		return nil, err
	}
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = fmt.Sprint(r.ID)
	}
	rows, err := s.db.QueryContext(ctx, eventSelect+` WHERE e.repo_id IN (`+strings.Join(ids, ",")+`) ORDER BY e.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows, false)
}

func (s *Store) Notifications(ctx context.Context, userID int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, COALESCE(e.repo_id,0), COALESCE(ou.name || '/' || r.name,''), COALESCE(a.name,'system'), e.kind, e.summary, e.created_at, n.read
		FROM notifications n JOIN events e ON e.id=n.event_id LEFT JOIN repos r ON r.id=e.repo_id LEFT JOIN users ou ON ou.id=r.owner_id LEFT JOIN users a ON a.id=e.actor_id
		WHERE n.user_id=? ORDER BY n.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows, true)
}

func (s *Store) UnreadCount(ctx context.Context, userID int64) int {
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE user_id=? AND read=0`, userID).Scan(&n)
	return n
}

func (s *Store) MarkNotificationsRead(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET read=1 WHERE user_id=?`, userID)
	return err
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run and job states.
const (
	StatusBlocked = "blocked"
	// StatusApproval: the run (and its jobs) waits for someone with write
	// access to approve it, e.g. a pull request from a fork by an outsider.
	StatusApproval  = "approval"
	StatusQueued    = "queued"
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailure   = "failure"
	StatusCancelled = "cancelled"
	StatusSkipped   = "skipped"
	StatusError     = "error"
)

func IsTerminal(status string) bool {
	switch status {
	case StatusSuccess, StatusFailure, StatusCancelled, StatusSkipped, StatusError:
		return true
	}
	return false
}

// ------------------------------------------------------------------ secrets
//
// The store keeps secret values opaque; encryption and name rules live in the
// forge service layer.

// SecretScope selects repository secrets (RepoID) or secrets of a user or
// organization (OwnerID) that its repositories inherit. Exactly one is set.
type SecretScope struct {
	RepoID  int64
	OwnerID int64
}

func (sc SecretScope) sql() (table, col string, id int64) {
	if sc.RepoID != 0 {
		return "secrets", "repo_id", sc.RepoID
	}
	return "owner_secrets", "owner_id", sc.OwnerID
}

// Binding is the AEAD additional data tying a ciphertext to its row.
func (sc SecretScope) Binding(name string) string {
	if sc.RepoID != 0 {
		return fmt.Sprintf("repo:%d:%s", sc.RepoID, name)
	}
	return fmt.Sprintf("owner:%d:%s", sc.OwnerID, name)
}

type Secret struct {
	Scope     SecretScope
	Name      string
	CreatedAt time.Time
	Value     string // as stored: ciphertext when Encrypted
	Encrypted bool
}

func (s *Store) SetSecret(ctx context.Context, sc SecretScope, name, value string, encrypted bool) error {
	table, col, id := sc.sql()
	_, err := s.db.ExecContext(ctx, `INSERT INTO `+table+`(`+col+`,name,value,encrypted,created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(`+col+`,name) DO UPDATE SET value=excluded.value, encrypted=excluded.encrypted, created_at=excluded.created_at`,
		id, name, value, encrypted, now())
	return err
}

// UpdateSecretValue rewrites a value in place (re-encryption) without
// touching its timestamp.
func (s *Store) UpdateSecretValue(ctx context.Context, sc SecretScope, name, value string, encrypted bool) error {
	table, col, id := sc.sql()
	_, err := s.db.ExecContext(ctx, `UPDATE `+table+` SET value=?, encrypted=? WHERE `+col+`=? AND name=?`, value, encrypted, id, name)
	return err
}

func (s *Store) DeleteSecret(ctx context.Context, sc SecretScope, name string) error {
	table, col, id := sc.sql()
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+`=? AND name=?`, id, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Secrets lists the secrets of one scope, sorted by name, values included.
func (s *Store) Secrets(ctx context.Context, sc SecretScope) ([]Secret, error) {
	table, col, id := sc.sql()
	out, err := s.scanSecrets(ctx, `SELECT 0, 0, name, value, encrypted, created_at FROM `+table+` WHERE `+col+`=? ORDER BY name`, id)
	for i := range out {
		out[i].Scope = sc
	}
	return out, err
}

// AllSecrets returns every secret of every scope (for key migration).
func (s *Store) AllSecrets(ctx context.Context) ([]Secret, error) {
	return s.scanSecrets(ctx, `SELECT repo_id, 0, name, value, encrypted, created_at FROM secrets
		UNION ALL SELECT 0, owner_id, name, value, encrypted, created_at FROM owner_secrets`)
}

func (s *Store) scanSecrets(ctx context.Context, q string, args ...any) ([]Secret, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		var sc Secret
		var ts int64
		if err := rows.Scan(&sc.Scope.RepoID, &sc.Scope.OwnerID, &sc.Name, &sc.Value, &sc.Encrypted, &ts); err != nil {
			return nil, err
		}
		sc.CreatedAt = time.Unix(ts, 0)
		out = append(out, sc)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ runners

type Runner struct {
	ID        int64
	Name      string
	Labels    []string
	Enabled   bool
	LastSeen  time.Time
	Version   string
	CreatedAt time.Time
}

func (r *Runner) Online() bool { return time.Since(r.LastSeen) < 30*time.Second }

const runnerCols = `id, name, labels, enabled, last_seen, version, created_at`

func scanRunner(sc interface{ Scan(...any) error }) (*Runner, error) {
	var r Runner
	var labels string
	var ls, c int64
	if err := sc.Scan(&r.ID, &r.Name, &labels, &r.Enabled, &ls, &r.Version, &c); err != nil {
		return nil, notFound(err)
	}
	r.Labels = splitList(labels)
	r.LastSeen, r.CreatedAt = time.Unix(ls, 0), time.Unix(c, 0)
	return &r, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// CreateRunner registers a runner and returns its plaintext auth token.
func (s *Store) CreateRunner(ctx context.Context, name string, labels []string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	tok, hash := RandomToken("fgr_")
	_, err := s.db.ExecContext(ctx, `INSERT INTO runners(name,token_hash,labels,created_at) VALUES (?,?,?,?)`, name, hash, strings.Join(labels, ","), now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return "", fmt.Errorf("runner %q already exists", name)
	}
	return tok, err
}

func (s *Store) Runners(ctx context.Context) ([]*Runner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runnerCols+` FROM runners ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Runner
	for rows.Next() {
		r, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RunnerByToken(ctx context.Context, token string) (*Runner, error) {
	return scanRunner(s.db.QueryRowContext(ctx, `SELECT `+runnerCols+` FROM runners WHERE token_hash=?`, HashToken(token)))
}

func (s *Store) RunnerSeen(ctx context.Context, id int64, version string, labels []string) {
	if labels != nil {
		s.db.ExecContext(ctx, `UPDATE runners SET last_seen=?, version=?, labels=? WHERE id=?`, now(), version, strings.Join(labels, ","), id)
		return
	}
	s.db.ExecContext(ctx, `UPDATE runners SET last_seen=? WHERE id=?`, now(), id)
}

func (s *Store) SetRunnerEnabled(ctx context.Context, id int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runners SET enabled=? WHERE id=?`, b2i(on), id)
	return err
}

func (s *Store) DeleteRunner(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runners WHERE id=?`, id)
	return err
}

// ------------------------------------------------------------------ runs

type Run struct {
	ID         int64
	RepoID     int64
	Number     int64
	Pipeline   string
	File       string
	Trigger    string
	Ref        string
	SHA        string
	PRNumber   int64
	Actor      string
	Status     string
	Error      string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

func (r *Run) Duration() time.Duration {
	if r.StartedAt.Unix() == 0 {
		return 0
	}
	end := r.FinishedAt
	if end.Unix() == 0 {
		end = time.Now()
	}
	return end.Sub(r.StartedAt).Round(time.Second)
}

type Job struct {
	ID         int64
	RunID      int64
	Key        string
	Name       string
	Spec       string // JSON
	Needs      []string
	Labels     []string
	Status     string
	RunnerID   int64
	Runner     string
	ExitCode   int
	CancelReq  bool
	Attempt    int
	Heartbeat  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

func (j *Job) Duration() time.Duration {
	if j.StartedAt.Unix() == 0 {
		return 0
	}
	end := j.FinishedAt
	if end.Unix() == 0 {
		end = time.Now()
	}
	return end.Sub(j.StartedAt).Round(time.Second)
}

const runSelect = `SELECT r.id, r.repo_id, r.number, r.pipeline, r.file, r.trigger, r.ref, r.sha, r.pr_number, COALESCE(u.name,''),
	r.status, r.error, r.created_at, r.started_at, r.finished_at FROM pipeline_runs r LEFT JOIN users u ON u.id=r.actor_id`

func scanRun(sc interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var c, st, f int64
	if err := sc.Scan(&r.ID, &r.RepoID, &r.Number, &r.Pipeline, &r.File, &r.Trigger, &r.Ref, &r.SHA, &r.PRNumber, &r.Actor,
		&r.Status, &r.Error, &c, &st, &f); err != nil {
		return nil, notFound(err)
	}
	r.CreatedAt, r.StartedAt, r.FinishedAt = time.Unix(c, 0), time.Unix(st, 0), time.Unix(f, 0)
	return &r, nil
}

const jobSelect = `SELECT j.id, j.run_id, j.key, j.name, j.spec, j.needs, j.labels, j.status, COALESCE(j.runner_id,0), COALESCE(rn.name,''),
	j.exit_code, j.cancel_req, j.attempt, j.heartbeat, j.started_at, j.finished_at FROM jobs j LEFT JOIN runners rn ON rn.id=j.runner_id`

func scanJob(sc interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var needs, labels string
	var hb, st, f int64
	if err := sc.Scan(&j.ID, &j.RunID, &j.Key, &j.Name, &j.Spec, &needs, &labels, &j.Status, &j.RunnerID, &j.Runner,
		&j.ExitCode, &j.CancelReq, &j.Attempt, &hb, &st, &f); err != nil {
		return nil, notFound(err)
	}
	j.Needs, j.Labels = splitList(needs), splitList(labels)
	j.Heartbeat, j.StartedAt, j.FinishedAt = time.Unix(hb, 0), time.Unix(st, 0), time.Unix(f, 0)
	return &j, nil
}

// NewJob is the input for CreateRun.
type NewJob struct {
	Key, Name, Spec string
	Needs, Labels   []string
}

func (s *Store) CreateRun(ctx context.Context, r *Run, actorID int64, jobs []NewJob) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM pipeline_runs WHERE repo_id=?`, r.RepoID).Scan(&n); err != nil {
		return nil, err
	}
	var actor any
	if actorID != 0 {
		actor = actorID
	}
	status := StatusPending
	if r.Status != "" {
		status = r.Status
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO pipeline_runs(repo_id,number,pipeline,file,trigger,ref,sha,pr_number,actor_id,status,error,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, r.RepoID, n, r.Pipeline, r.File, r.Trigger, r.Ref, r.SHA, r.PRNumber, actor, status, r.Error, now())
	if err != nil {
		return nil, err
	}
	runID, _ := res.LastInsertId()
	for _, j := range jobs {
		st := StatusQueued
		if len(j.Needs) > 0 {
			st = StatusBlocked
		}
		if status == StatusApproval {
			st = StatusApproval
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(run_id,key,name,spec,needs,labels,status) VALUES (?,?,?,?,?,?,?)`,
			runID, j.Key, j.Name, j.Spec, strings.Join(j.Needs, ","), strings.Join(j.Labels, ","), st); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.RunByID(ctx, runID)
}

// ApproveRun releases a run waiting for approval: its jobs become queued
// (or blocked on their needs). ErrNotFound if it isn't waiting.
func (s *Store) ApproveRun(ctx context.Context, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE pipeline_runs SET status=? WHERE id=? AND status=?`, StatusPending, runID, StatusApproval)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = CASE WHEN needs='' THEN 'queued' ELSE 'blocked' END
		WHERE run_id=? AND status=?`, runID, StatusApproval); err != nil {
		return err
	}
	return tx.Commit()
}

// HeldRuns returns ids of a pull request's runs of one pipeline still
// waiting for approval.
func (s *Store) HeldRuns(ctx context.Context, repoID, prNumber int64, pipeline string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM pipeline_runs WHERE repo_id=? AND pr_number=? AND pipeline=? AND status=?`,
		repoID, prNumber, pipeline, StatusApproval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) RunByID(ctx context.Context, id int64) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE r.id=?`, id))
}

func (s *Store) RunByNumber(ctx context.Context, repoID, n int64) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE r.repo_id=? AND r.number=?`, repoID, n))
}

func (s *Store) Runs(ctx context.Context, repoID int64, limit int) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE r.repo_id=? ORDER BY r.id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRunForSHA returns the most recent run status for a commit (for PR checks).
func (s *Store) RunsForSHA(ctx context.Context, repoID int64, sha string) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE r.repo_id=? AND r.sha=? ORDER BY r.id DESC`, repoID, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Jobs(ctx context.Context, runID int64) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE j.run_id=? ORDER BY j.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) JobByID(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobSelect+` WHERE j.id=?`, id))
}

// ClaimJob atomically assigns the oldest queued job whose label requirements
// are a subset of the runner's labels.
func (s *Store) ClaimJob(ctx context.Context, runnerID int64, runnerLabels []string) (*Job, error) {
	have := map[string]bool{}
	for _, l := range runnerLabels {
		have[l] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, labels FROM jobs WHERE status='queued' ORDER BY id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	var pick int64
	for rows.Next() && pick == 0 {
		var id int64
		var labels string
		if err := rows.Scan(&id, &labels); err != nil {
			rows.Close()
			return nil, err
		}
		ok := true
		for _, l := range splitList(labels) {
			if !have[l] {
				ok = false
				break
			}
		}
		if ok {
			pick = id
		}
	}
	rows.Close()
	if pick == 0 {
		return nil, ErrNotFound
	}
	t := now()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='running', runner_id=?, started_at=?, heartbeat=?, finished_at=0, exit_code=0 WHERE id=? AND status='queued'`,
		runnerID, t, t, pick); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_runs SET status='running', started_at=CASE WHEN started_at=0 THEN ? ELSE started_at END
		WHERE id=(SELECT run_id FROM jobs WHERE id=?) AND status IN ('pending','running')`, t, pick); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.JobByID(ctx, pick)
}

// Heartbeat records liveness and reports whether cancellation was requested.
func (s *Store) Heartbeat(ctx context.Context, jobID int64) (cancel bool, err error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET heartbeat=? WHERE id=?`, now(), jobID); err != nil {
		return false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT cancel_req FROM jobs WHERE id=?`, jobID).Scan(&cancel)
	return cancel, notFound(err)
}

func (s *Store) FinishJob(ctx context.Context, jobID int64, status string, exitCode int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?, exit_code=?, finished_at=? WHERE id=? AND status='running'`, status, exitCode, now(), jobID)
	return err
}

func (s *Store) SetJobStatus(ctx context.Context, jobID int64, status string) error {
	q := `UPDATE jobs SET status=? WHERE id=?`
	if IsTerminal(status) {
		q = `UPDATE jobs SET status=?, finished_at=CASE WHEN started_at>0 THEN ` + fmt.Sprint(now()) + ` ELSE 0 END WHERE id=?`
	}
	_, err := s.db.ExecContext(ctx, q, status, jobID)
	return err
}

func (s *Store) SetRunStatus(ctx context.Context, runID int64, status, errMsg string) error {
	fin := int64(0)
	if IsTerminal(status) {
		fin = now()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE pipeline_runs SET status=?, error=CASE WHEN ?<>'' THEN ? ELSE error END, finished_at=? WHERE id=?`,
		status, errMsg, errMsg, fin, runID)
	return err
}

// CancelRun marks waiting jobs cancelled and asks running ones to stop.
func (s *Store) CancelRun(ctx context.Context, runID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status='cancelled' WHERE run_id=? AND status IN ('queued','blocked','approval')`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET cancel_req=1 WHERE run_id=? AND status='running'`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetJobsForRetry re-queues failed/cancelled jobs (or all jobs) of a run.
func (s *Store) ResetJobsForRetry(ctx context.Context, runID int64, failedOnly bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cond := `status NOT IN ('running')`
	if failedOnly {
		cond = `status IN ('failure','cancelled','error','skipped')`
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status=CASE WHEN needs='' THEN 'queued' ELSE 'blocked' END, cancel_req=0, attempt=attempt+1,
		runner_id=NULL, exit_code=0, started_at=0, finished_at=0 WHERE run_id=? AND `+cond, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE pipeline_runs SET status='pending', error='', finished_at=0 WHERE id=?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// StaleJobs returns running jobs whose heartbeat is older than d.
func (s *Store) StaleJobs(ctx context.Context, d time.Duration) ([]*Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE j.status='running' AND j.heartbeat<?`, time.Now().Add(-d).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ActiveRuns returns runs that are not yet terminal (used by the scheduler).
func (s *Store) ActiveRunIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM pipeline_runs WHERE status IN ('pending','running')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ artifacts

type Artifact struct {
	ID        int64
	JobID     int64
	Name      string
	Size      int64
	Path      string
	CreatedAt time.Time
}

func (s *Store) AddArtifact(ctx context.Context, a *Artifact) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO artifacts(job_id,name,size,path,created_at) VALUES (?,?,?,?,?)`, a.JobID, a.Name, a.Size, a.Path, now())
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) RunArtifacts(ctx context.Context, runID int64) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.job_id, a.name, a.size, a.path, a.created_at FROM artifacts a JOIN jobs j ON j.id=a.job_id
		WHERE j.run_id=? ORDER BY a.id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		var ts int64
		if err := rows.Scan(&a.ID, &a.JobID, &a.Name, &a.Size, &a.Path, &ts); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(ts, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ArtifactByID(ctx context.Context, id int64) (*Artifact, error) {
	var a Artifact
	var ts int64
	err := s.db.QueryRowContext(ctx, `SELECT id, job_id, name, size, path, created_at FROM artifacts WHERE id=?`, id).
		Scan(&a.ID, &a.JobID, &a.Name, &a.Size, &a.Path, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	a.CreatedAt = time.Unix(ts, 0)
	return &a, err
}

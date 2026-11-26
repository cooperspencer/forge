package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/rs/zerolog"
)

// Orchestrator turns repository events into pipeline runs and advances job
// state machines. It never executes anything itself.
type Orchestrator struct {
	Store    *store.Store
	Logs     *LogStore
	Broker   *Broker
	RepoPath func(*store.Repo) string
	Log      zerolog.Logger

	// StaleAfter is how long a running job may go without a heartbeat before
	// it is failed (runner crashed or lost connectivity).
	StaleAfter time.Duration
}

// Trigger describes why runs should be created.
type Trigger struct {
	Kind       string // push | tag | pull_request | manual | schedule
	Ref        string // full ref (refs/heads/x, refs/tags/v1, refs/pull/3/head)
	SHA        string
	PRNumber   int64
	BaseBranch string // pull_request target branch
	FromFork   bool   // PR from a fork: secrets are withheld
	// Hold: the code comes from someone without write access to the
	// repository; runs wait for approval before any job is queued.
	Hold         bool
	ActorID      int64
	ChangedFiles []string
	Only         string // manual: restrict to a single pipeline name (optional)
}

// LoadPipelines reads all pipeline definitions at sha.
func LoadPipelines(ctx context.Context, g *gitx.Repo, sha string) (map[string]*Pipeline, map[string]error) {
	pipes := map[string]*Pipeline{}
	errs := map[string]error{}
	var files []string
	if _, _, err := g.Blob(ctx, sha, RootFile, 1); err == nil {
		files = append(files, RootFile)
	}
	if list, err := g.ListFiles(ctx, sha, PipelineDir); err == nil {
		for _, f := range list {
			if IsPipelineFile(f) {
				files = append(files, f)
			}
		}
	}
	for _, f := range files {
		data, _, err := g.Blob(ctx, sha, f, 1<<20)
		if err != nil {
			errs[f] = err
			continue
		}
		p, err := Parse(data)
		if err != nil {
			errs[f] = err
			continue
		}
		pipes[f] = p
	}
	return pipes, errs
}

func pipelineName(file string, p *Pipeline) string {
	if p != nil && p.Name != "" {
		return p.Name
	}
	if file == RootFile {
		return "default"
	}
	base := file[strings.LastIndex(file, "/")+1:]
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
}

// Dispatch creates runs for every pipeline matching t. It returns the runs created.
func (o *Orchestrator) Dispatch(ctx context.Context, repo *store.Repo, t Trigger) ([]*store.Run, error) {
	g := gitx.Open(o.RepoPath(repo))
	pipes, errs := LoadPipelines(ctx, g, t.SHA)
	ev := Event{Kind: t.Kind, Ref: t.Ref, BaseBranch: t.BaseBranch, ChangedFiles: t.ChangedFiles}
	var runs []*store.Run
	for file, perr := range errs {
		if t.Only != "" && t.Only != file && t.Only != pipelineName(file, nil) {
			continue
		}
		// Surface broken definitions as errored runs so authors notice.
		r, err := o.Store.CreateRun(ctx, &store.Run{RepoID: repo.ID, Pipeline: pipelineName(file, nil), File: file, Trigger: t.Kind,
			Ref: t.Ref, SHA: t.SHA, PRNumber: t.PRNumber, Status: store.StatusError, Error: perr.Error()}, t.ActorID, nil)
		if err == nil {
			o.Store.SetRunStatus(ctx, r.ID, store.StatusError, perr.Error())
			runs = append(runs, r)
		}
	}
	for file, p := range pipes {
		name := pipelineName(file, p)
		if t.Only != "" && t.Only != name && t.Only != file {
			continue
		}
		if t.Kind == "manual" && t.Only != "" {
			// explicit manual trigger of a named pipeline always runs
		} else if !p.Matches(ev) {
			continue
		}
		specs := p.Expand(name)
		var jobs []store.NewJob
		for _, s := range specs {
			if t.FromFork {
				s.Secrets, s.Fork = nil, true
			}
			b, _ := json.Marshal(s)
			jobs = append(jobs, store.NewJob{Key: s.Key, Name: s.Name, Spec: string(b), Needs: s.Needs, Labels: s.Labels})
		}
		status := ""
		if t.Hold {
			status = store.StatusApproval
			// a newer push supersedes runs still waiting for approval
			if old, _ := o.Store.HeldRuns(ctx, repo.ID, t.PRNumber, name); len(old) > 0 {
				for _, id := range old {
					o.Cancel(ctx, id)
				}
			}
		}
		r, err := o.Store.CreateRun(ctx, &store.Run{RepoID: repo.ID, Pipeline: name, File: file, Trigger: t.Kind,
			Ref: t.Ref, SHA: t.SHA, PRNumber: t.PRNumber, Status: status}, t.ActorID, jobs)
		if err != nil {
			return runs, err
		}
		runs = append(runs, r)
		if t.Hold {
			o.Store.RecordEvent(ctx, repo.ID, t.ActorID, "pipeline", fmt.Sprintf("pipeline %s #%d for PR #%d (%s) is waiting for approval: it runs code from a fork",
				name, r.Number, t.PRNumber, short(t.SHA)))
			continue
		}
		o.Store.RecordEvent(ctx, repo.ID, t.ActorID, "pipeline", fmt.Sprintf("pipeline %s #%d started for %s (%s)", name, r.Number, shortRef(t.Ref), short(t.SHA)))
	}
	if len(runs) > 0 {
		o.Broker.Publish(fmt.Sprintf("runs:%d", repo.ID))
	}
	return runs, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func shortRef(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
	}
	return ref
}

// Advance recomputes blocked jobs and the overall run status. It is
// idempotent and safe to call after any job transition.
func (o *Orchestrator) Advance(ctx context.Context, runID int64) error {
	run, err := o.Store.RunByID(ctx, runID)
	if err != nil {
		return err
	}
	for pass := 0; pass < 8; pass++ { // unblocking can cascade (skips)
		jobs, err := o.Store.Jobs(ctx, runID)
		if err != nil {
			return err
		}
		byKey := map[string][]*store.Job{}
		for _, j := range jobs {
			byKey[j.Key] = append(byKey[j.Key], j)
		}
		changed := false
		for _, j := range jobs {
			if j.Status != store.StatusBlocked {
				continue
			}
			var spec JobSpec
			json.Unmarshal([]byte(j.Spec), &spec)
			ready, failed := true, false
			for _, need := range j.Needs {
				for _, dep := range byKey[need] {
					if !store.IsTerminal(dep.Status) {
						ready = false
					} else if dep.Status != store.StatusSuccess {
						failed = true
					}
				}
			}
			if !ready {
				continue
			}
			next := store.StatusQueued
			if failed && !spec.Always {
				next = store.StatusSkipped
			}
			o.Store.SetJobStatus(ctx, j.ID, next)
			changed = true
		}
		if !changed {
			break
		}
	}
	jobs, err := o.Store.Jobs(ctx, runID)
	if err != nil {
		return err
	}
	status := aggregate(jobs)
	if status != run.Status {
		o.Store.SetRunStatus(ctx, runID, status, "")
		if store.IsTerminal(status) {
			o.Store.RecordEvent(ctx, run.RepoID, 0, "pipeline", fmt.Sprintf("pipeline %s #%d %s", run.Pipeline, run.Number, status))
		}
	}
	o.Broker.Publish(fmt.Sprintf("run:%d", runID))
	o.Broker.Publish(fmt.Sprintf("runs:%d", run.RepoID))
	return nil
}

func aggregate(jobs []*store.Job) string {
	if len(jobs) == 0 {
		return store.StatusError
	}
	running, waiting, approval := false, false, false
	var failure, cancelled, errored bool
	for _, j := range jobs {
		switch j.Status {
		case store.StatusApproval:
			approval = true
		case store.StatusRunning:
			running = true
		case store.StatusQueued, store.StatusBlocked:
			waiting = true
		case store.StatusFailure:
			failure = true
		case store.StatusCancelled:
			cancelled = true
		case store.StatusError:
			errored = true
		}
	}
	switch {
	case approval:
		return store.StatusApproval
	case running:
		return store.StatusRunning
	case waiting:
		// Some jobs done, others queued: still "running" from the user's view.
		for _, j := range jobs {
			if store.IsTerminal(j.Status) {
				return store.StatusRunning
			}
		}
		return store.StatusPending
	case errored:
		return store.StatusError
	case failure:
		return store.StatusFailure
	case cancelled:
		return store.StatusCancelled
	}
	return store.StatusSuccess
}

// JobFinished is called by the runner API when a job completes.
func (o *Orchestrator) JobFinished(ctx context.Context, jobID int64, status string, exitCode int) error {
	if err := o.Store.FinishJob(ctx, jobID, status, exitCode); err != nil {
		return err
	}
	j, err := o.Store.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	return o.Advance(ctx, j.RunID)
}

// Approve releases a run that is waiting for approval.
func (o *Orchestrator) Approve(ctx context.Context, runID int64) error {
	if err := o.Store.ApproveRun(ctx, runID); err != nil {
		return err
	}
	return o.Advance(ctx, runID) // publishes, which also wakes waiting runners
}

func (o *Orchestrator) Cancel(ctx context.Context, runID int64) error {
	if err := o.Store.CancelRun(ctx, runID); err != nil {
		return err
	}
	return o.Advance(ctx, runID)
}

// Rerun re-queues jobs of a finished run. failedOnly re-runs only failed jobs
// and their (skipped) dependents.
func (o *Orchestrator) Rerun(ctx context.Context, runID int64, failedOnly bool) error {
	run, err := o.Store.RunByID(ctx, runID)
	if err != nil {
		return err
	}
	if !store.IsTerminal(run.Status) {
		return fmt.Errorf("run is still in progress")
	}
	jobs, _ := o.Store.Jobs(ctx, runID)
	if len(jobs) == 0 {
		return fmt.Errorf("run has no jobs (definition error?)")
	}
	for _, j := range jobs {
		if !failedOnly || (j.Status != store.StatusSuccess) {
			o.Logs.Reset(j.ID)
		}
	}
	if err := o.Store.ResetJobsForRetry(ctx, runID, failedOnly); err != nil {
		return err
	}
	return o.Advance(ctx, runID)
}

// Reap fails running jobs whose runner stopped heartbeating. Run it periodically.
func (o *Orchestrator) Reap(ctx context.Context) {
	d := o.StaleAfter
	if d == 0 {
		d = 90 * time.Second
	}
	jobs, err := o.Store.StaleJobs(ctx, d)
	if err != nil {
		o.Log.Error().Err(err).Msg("reap")
		return
	}
	for _, j := range jobs {
		o.Logs.Append(j.ID, []byte(fmt.Sprintf("\n\x1b[31m[forge] runner %q stopped responding; job marked as error\x1b[0m\n", j.Runner)))
		o.JobFinished(ctx, j.ID, store.StatusError, -1)
	}
	// Sweep runs whose jobs were cancelled while blocked etc.
	ids, _ := o.Store.ActiveRunIDs(ctx)
	for _, id := range ids {
		o.Advance(ctx, id)
	}
}

func (o *Orchestrator) Loop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.Reap(ctx)
		}
	}
}

package forge

import (
	"context"
	"sync"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/store"
)

// scheduleCache keeps parsed pipelines per repository, keyed by the default
// branch's commit, so idle repositories are not re-read every minute.
type scheduleCache struct {
	mu    sync.Mutex
	repos map[int64]scheduleEntry
}

type scheduleEntry struct {
	sha   string
	pipes map[string]*ci.Pipeline // file → pipeline, only those with schedules
}

// ScheduleLoop fires scheduled pipelines at the start of every minute until
// ctx ends. Missed minutes (server down) are not caught up, like cron.
func (f *Forge) ScheduleLoop(ctx context.Context) {
	for {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		if n, err := f.RunSchedules(ctx, next); err != nil {
			f.Log.Error().Err(err).Msg("scheduled pipelines")
		} else if n > 0 {
			f.Log.Debug().Int("runs", n).Msg("scheduled pipelines started")
		}
	}
}

// RunSchedules starts every pipeline whose schedule matches the minute of t,
// on the head of each repository's default branch. Forks are skipped (their
// schedules would duplicate the parent's). It returns the number of runs.
func (f *Forge) RunSchedules(ctx context.Context, t time.Time) (int, error) {
	repos, err := f.Store.VisibleRepos(ctx, &store.User{IsAdmin: true})
	if err != nil {
		return 0, err
	}
	f.sched.mu.Lock()
	defer f.sched.mu.Unlock()
	if f.sched.repos == nil {
		f.sched.repos = map[int64]scheduleEntry{}
	}
	seen := map[int64]bool{}
	started := 0
	for _, r := range repos {
		if r.ForkOf != 0 {
			continue
		}
		seen[r.ID] = true
		ref := "refs/heads/" + r.DefaultBranch
		sha, err := f.Git(r).ResolveRef(ctx, ref)
		if err != nil {
			continue // empty repository
		}
		e, ok := f.sched.repos[r.ID]
		if !ok || e.sha != sha {
			pipes, _ := ci.LoadPipelines(ctx, f.Git(r), sha)
			e = scheduleEntry{sha: sha, pipes: map[string]*ci.Pipeline{}}
			for file, p := range pipes {
				if p.On != nil && len(p.On.Schedule) > 0 {
					e.pipes[file] = p
				}
			}
			f.sched.repos[r.ID] = e
		}
		for file, p := range e.pipes {
			if !p.DueAt(t) {
				continue
			}
			runs, err := f.CI.Dispatch(ctx, r, ci.Trigger{Kind: "schedule", Ref: ref, SHA: sha, Only: file})
			if err != nil {
				f.Log.Error().Err(err).Str("repo", r.FullName()).Str("file", file).Msg("scheduled pipeline")
				continue
			}
			started += len(runs)
			for _, run := range runs {
				f.Log.Info().Str("repo", r.FullName()).Str("pipeline", run.Pipeline).Int64("run", run.Number).Msg("scheduled run started")
			}
		}
	}
	for id := range f.sched.repos {
		if !seen[id] {
			delete(f.sched.repos, id)
		}
	}
	return started, nil
}

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/store"
)

// Runner protocol (all requests carry `Authorization: Bearer <runner token>`):
//
//	POST /api/v1/runner/claim              {labels, version} -> 200 JobPayload | 204 no work
//	POST /api/v1/runner/jobs/{id}/log      raw bytes appended to the job log
//	POST /api/v1/runner/jobs/{id}/heartbeat -> {"cancel": bool}
//	POST /api/v1/runner/jobs/{id}/finish   {status, exit_code}
//	POST /api/v1/runner/jobs/{id}/artifacts?name=x  raw bytes
//
// Claim long-polls for up to ~25s so idle runners don't hammer the server.

type runnerFunc func(w http.ResponseWriter, r *http.Request, rn *store.Runner) error

func (s *Server) runnerAuth(fn runnerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		rn, err := s.F.Store.RunnerByToken(r.Context(), tok)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: invalid runner token", errUnauthorized))
			return
		}
		if err := fn(w, r, rn); err != nil {
			writeErr(w, err)
		}
	}
}

func (s *Server) runnerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/runner/claim", s.runnerAuth(s.claim))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/log", s.runnerAuth(s.appendLog))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/heartbeat", s.runnerAuth(s.heartbeat))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/finish", s.runnerAuth(s.finish))
	mux.HandleFunc("POST /api/v1/runner/jobs/{id}/artifacts", s.runnerAuth(s.uploadArtifact))
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request, rn *store.Runner) error {
	var req struct {
		Labels  []string `json:"labels"`
		Version string   `json:"version"`
		Wait    bool     `json:"wait"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	labels := req.Labels
	if labels == nil {
		labels = rn.Labels
	}
	s.F.Store.RunnerSeen(r.Context(), rn.ID, req.Version, req.Labels)
	if !rn.Enabled {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	var deadline <-chan time.Time
	if req.Wait {
		t := time.NewTimer(25 * time.Second)
		defer t.Stop()
		deadline = t.C
	}
	sub, unsub := s.F.Broker.Subscribe()
	defer unsub()
	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	for {
		j, err := s.F.Store.ClaimJob(r.Context(), rn.ID, labels)
		if err == nil {
			p, err := s.payload(r.Context(), j)
			if err != nil {
				s.F.Logs.Append(j.ID, []byte("[forge] failed to prepare job: "+err.Error()+"\n"))
				s.F.CI.JobFinished(r.Context(), j.ID, store.StatusError, -1)
				return err
			}
			s.F.Logs.Append(j.ID, []byte(fmt.Sprintf("\x1b[2m[forge] job picked up by runner %s at %s\x1b[0m\n", rn.Name, time.Now().Format(time.RFC3339))))
			s.F.CI.Advance(r.Context(), j.RunID)
			writeJSON(w, 200, p)
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if deadline == nil {
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		select {
		case <-deadline:
			w.WriteHeader(http.StatusNoContent)
			return nil
		case <-r.Context().Done():
			return nil
		case <-sub: // something changed (new run, job finished) — try again
		case <-poll.C:
			s.F.Store.RunnerSeen(r.Context(), rn.ID, req.Version, nil)
		}
	}
}

func (s *Server) payload(ctx context.Context, j *store.Job) (*ci.JobPayload, error) {
	run, err := s.F.Store.RunByID(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	repo, err := s.F.Store.RepoByID(ctx, run.RepoID)
	if err != nil {
		return nil, err
	}
	var spec ci.JobSpec
	if err := json.Unmarshal([]byte(j.Spec), &spec); err != nil {
		return nil, err
	}
	secrets, missing, err := s.F.JobSecrets(ctx, repo, spec.Secrets)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		s.F.Logs.Append(j.ID, []byte(fmt.Sprintf("\x1b[33m[forge] warning: secrets not defined for %s or %s: %s\x1b[0m\n",
			repo.FullName(), repo.Owner, strings.Join(missing, ", "))))
	}
	branch := ""
	if strings.HasPrefix(run.Ref, "refs/heads/") {
		branch = strings.TrimPrefix(run.Ref, "refs/heads/")
	}
	tag := ""
	if strings.HasPrefix(run.Ref, "refs/tags/") {
		tag = strings.TrimPrefix(run.Ref, "refs/tags/")
	}
	env := map[string]string{
		"CI":               "true",
		"FORGE":            "true",
		"FORGE_REPO":       repo.FullName(),
		"FORGE_REF":        run.Ref,
		"FORGE_BRANCH":     branch,
		"FORGE_TAG":        tag,
		"FORGE_SHA":        run.SHA,
		"FORGE_TRIGGER":    run.Trigger,
		"FORGE_PIPELINE":   run.Pipeline,
		"FORGE_RUN_NUMBER": strconv.FormatInt(run.Number, 10),
		"FORGE_JOB":        spec.Key,
		"FORGE_JOB_ID":     strconv.FormatInt(j.ID, 10),
		"FORGE_ACTOR":      run.Actor,
		"FORGE_SERVER_URL": s.F.Cfg.HTTP.PublicURL,
		"FORGE_REGISTRY":   s.F.Cfg.RegistryHost(),
		"FORGE_PR_NUMBER":  "",
	}
	if run.PRNumber > 0 {
		env["FORGE_PR_NUMBER"] = strconv.FormatInt(run.PRNumber, 10)
	}
	return &ci.JobPayload{
		JobID: j.ID, RunID: run.ID, RunNumber: run.Number, Repo: repo.FullName(),
		CloneURL: s.F.Cfg.HTTPCloneURL(repo.FullName()), CloneUser: "forge-job", CloneToken: s.F.JobToken(j.ID), LFS: repo.LFS,
		Ref: run.Ref, SHA: run.SHA, Spec: spec, Secrets: secrets, Env: env,
	}, nil
}

// ownJob verifies that the job is running on this runner.
func (s *Server) ownJob(r *http.Request, rn *store.Runner) (*store.Job, error) {
	id, err := pathInt(r, "id")
	if err != nil {
		return nil, err
	}
	j, err := s.F.Store.JobByID(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if j.RunnerID != rn.ID {
		return nil, fmt.Errorf("job %d is not assigned to this runner", id)
	}
	if j.Status != store.StatusRunning {
		return j, errJobNotRunning
	}
	return j, nil
}

var errJobNotRunning = errors.New("job is no longer running")

func (s *Server) appendLog(w http.ResponseWriter, r *http.Request, rn *store.Runner) error {
	j, err := s.ownJob(r, rn)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if err := s.F.Logs.Append(j.ID, b); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, rn *store.Runner) error {
	j, err := s.ownJob(r, rn)
	if errors.Is(err, errJobNotRunning) {
		// e.g. reaped as stale: tell the runner to stop
		writeJSON(w, 200, map[string]bool{"cancel": true})
		return nil
	}
	if err != nil {
		return err
	}
	s.F.Store.RunnerSeen(r.Context(), rn.ID, rn.Version, nil)
	cancel, err := s.F.Store.Heartbeat(r.Context(), j.ID)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]bool{"cancel": cancel})
	return nil
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request, rn *store.Runner) error {
	j, err := s.ownJob(r, rn)
	if err != nil {
		return err
	}
	var req struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	switch req.Status {
	case store.StatusSuccess, store.StatusFailure, store.StatusCancelled, store.StatusError:
	default:
		return fmt.Errorf("invalid status %q", req.Status)
	}
	if err := s.F.CI.JobFinished(r.Context(), j.ID, req.Status, req.ExitCode); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request, rn *store.Runner) error {
	j, err := s.ownJob(r, rn)
	if err != nil {
		return err
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." || name == "/" {
		return fmt.Errorf("artifact name required")
	}
	dir := s.F.ArtifactDir(j.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	max := int64(s.F.Cfg.CI.MaxArtifactMB) << 20
	n, err := io.Copy(f, io.LimitReader(r.Body, max+1))
	f.Close()
	if err != nil {
		os.Remove(path)
		return err
	}
	if n > max {
		os.Remove(path)
		return fmt.Errorf("artifact exceeds %d MB", s.F.Cfg.CI.MaxArtifactMB)
	}
	a := &store.Artifact{JobID: j.ID, Name: name, Size: n, Path: path}
	if err := s.F.Store.AddArtifact(r.Context(), a); err != nil {
		return err
	}
	writeJSON(w, 201, map[string]int64{"id": a.ID})
	return nil
}

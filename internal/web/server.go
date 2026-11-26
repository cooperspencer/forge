// Package web serves git over smart HTTP, the JSON REST API (/api/v1) and
// the runner protocol (/api/v1/runner). There is deliberately no HTML UI:
// the TUI is the interface; HTTP exists for git clients, scripts and runners.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/rs/zerolog"
)

type Server struct {
	F   *forge.Forge
	srv *http.Server
}

func New(f *forge.Forge) *Server {
	s := &Server{F: f}
	mux := http.NewServeMux()
	s.routes(mux)
	s.srv = &http.Server{Addr: f.Cfg.HTTP.Listen, Handler: s.logging(mux), ReadHeaderTimeout: 15 * time.Second}
	return s
}

func (s *Server) Handler() http.Handler { return s.srv.Handler }

func (s *Server) ListenAndServe() error {
	err := s.srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		// runner polling is noisy; only log failures there
		if strings.HasPrefix(r.URL.Path, "/api/v1/runner/") && sw.code < 400 {
			return
		}
		s.F.Log.Debug().Str("method", r.Method).Str("path", r.URL.Path).Int("status", sw.code).Dur("dur_ms", time.Since(t)).Msg("http")
	})
}

// ---------------------------------------------------------------- auth

type principal struct {
	user  *store.User
	jobID int64 // non-zero for job-scoped clone tokens
}

// authenticate accepts `Authorization: Bearer <token>` or HTTP Basic with the
// token as password (what git clients send). Job clone tokens use the
// username "forge-job".
func (s *Server) authenticate(r *http.Request) (principal, error) {
	var tok, user string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	} else if u, p, ok := r.BasicAuth(); ok {
		user, tok = u, p
	}
	if tok == "" {
		return principal{}, nil
	}
	if user == "forge-job" {
		if id, ok := s.F.VerifyJobToken(tok); ok {
			return principal{jobID: id}, nil
		}
		return principal{}, errors.New("invalid job token")
	}
	u, err := s.F.Store.UserByToken(r.Context(), tok)
	if errors.Is(err, store.ErrTokenExpired) {
		return principal{}, err
	} else if err != nil {
		return principal{}, errors.New("invalid token")
	}
	return principal{user: u}, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, forge.ErrForbidden), errors.Is(err, store.ErrScope):
		code = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errUnauthorized):
		code = http.StatusUnauthorized
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

var errUnauthorized = errors.New("authentication required")

// ---------------------------------------------------------------- git smart HTTP

func (s *Server) isGitRequest(r *http.Request) (repo, rest string, ok bool) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	for _, suffix := range []string{"/info/refs", "/git-upload-pack", "/git-receive-pack", "/HEAD", "/objects/"} {
		if i := strings.Index(p, suffix); i > 0 {
			return strings.TrimSuffix(p[:i], ".git"), p[i:], true
		}
	}
	return "", "", false
}

func (s *Server) serveGit(w http.ResponseWriter, r *http.Request, full, rest string) {
	ctx := r.Context()
	pr, err := s.authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	push := rest == "/git-receive-pack" || r.URL.Query().Get("service") == "git-receive-pack"
	need := store.AccessRead
	if push {
		need = store.AccessWrite
	}
	full, isWiki := cutWiki(full)
	// a token without the code scope gets what an anonymous client gets
	var scopeErr error
	if pr.user != nil {
		if scopeErr = pr.user.Grant.Require(store.ScopeCode, need); scopeErr != nil {
			pr.user = nil
		}
	}
	var repo *store.Repo
	if pr.jobID != 0 {
		// job tokens: read-only, only for the job's repository, only while running
		repo, err = s.jobRepo(ctx, pr.jobID)
		if err == nil && (push || !strings.EqualFold(repo.FullName(), full)) {
			err = forge.ErrForbidden
		}
	} else {
		repo, _, err = s.F.ResolveRepo(ctx, pr.user, full, need)
	}
	if err != nil {
		if scopeErr != nil {
			http.Error(w, scopeErr.Error(), http.StatusForbidden)
			return
		}
		if pr.user == nil && pr.jobID == 0 {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		code := http.StatusNotFound
		if errors.Is(err, forge.ErrForbidden) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	repoPath := s.F.RepoPath(repo)
	g := s.F.Git(repo)
	if push && repo.Mirror && !isWiki {
		http.Error(w, s.F.ReadOnlyError(ctx, repo).Error(), http.StatusForbidden)
		return
	}
	if isWiki {
		repoPath = s.F.WikiPath(repo)
		if push {
			if err := s.F.EnsureWiki(ctx, repo); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		} else if _, err := os.Stat(repoPath); err != nil {
			http.Error(w, "the wiki of "+repo.FullName()+" has no pages yet", http.StatusNotFound)
			return
		}
		g = gitx.Open(repoPath)
	}
	root := filepath.Dir(filepath.Dir(repoPath))
	rel, _ := filepath.Rel(root, repoPath)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git not installed", 500)
		return
	}
	remote := "anonymous"
	if pr.user != nil {
		remote = pr.user.Name
	}
	h := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Dir:  root,
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"PATH_INFO=/" + filepath.ToSlash(rel) + rest,
			"REMOTE_USER=" + remote,
		},
		InheritEnv: []string{"PATH", "HOME"},
	}
	gitCfg := []string{"receive.hideRefs=refs/pull", "http.receivepack=true"}
	if push && !isWiki && pr.user != nil {
		cfg, env := s.F.PushHook(ctx, pr.user, repo) // branch protection
		gitCfg = append(gitCfg, cfg...)
		h.Env = append(h.Env, env...)
	}
	h.Env = append(h.Env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(gitCfg)))
	for i, kv := range gitCfg {
		k, v, _ := strings.Cut(kv, "=")
		h.Env = append(h.Env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, v))
	}
	glog := s.F.Log.With().Str("repo", repo.FullName()).Logger()
	h.Logger = log.New(logWriter{glog, zerolog.ErrorLevel}, "", 0)
	h.Stderr = logWriter{glog, zerolog.WarnLevel}
	if p := r.Header.Get("Git-Protocol"); p != "" {
		h.Env = append(h.Env, "GIT_PROTOCOL="+p)
	}
	if rest == "/git-receive-pack" {
		before, _ := g.RefMap(ctx)
		h.ServeHTTP(w, r)
		after, _ := g.RefMap(context.Background())
		if ups := forge.DiffRefs(before, after); len(ups) > 0 && isWiki {
			s.F.WikiPostPush(context.Background(), pr.user, repo)
		} else if len(ups) > 0 {
			s.F.PostReceive(context.Background(), pr.user, repo, ups)
		}
		return
	}
	h.ServeHTTP(w, r)
}

func (s *Server) jobRepo(ctx context.Context, jobID int64) (*store.Repo, error) {
	j, err := s.F.Store.JobByID(ctx, jobID)
	if err != nil || j.Status != store.StatusRunning {
		return nil, forge.ErrForbidden
	}
	run, err := s.F.Store.RunByID(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	return s.F.Store.RepoByID(ctx, run.RepoID)
}

// ---------------------------------------------------------------- routing

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if repo, rest, ok := isLFSRequest(r.URL.Path); ok {
			s.serveLFS(w, r, repo, rest)
			return
		}
		if repo, rest, ok := s.isGitRequest(r); ok {
			s.serveGit(w, r, repo, rest)
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s\n\nThis forge is terminal-first. Connect with:\n\n    %s\n\nAPI: %s/api/v1  (docs: /api/v1/docs)\n",
				s.F.Cfg.Name, s.F.Cfg.SSHCommand(), s.F.Cfg.HTTP.PublicURL)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/v1/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(APIDocs))
	})
	s.apiRoutes(mux)
	s.runnerRoutes(mux)
	if s.F.Cfg.Packages.Enabled {
		s.packageRoutes(mux)
	}
}

// handler wraps API handlers with authentication.
type apiFunc func(w http.ResponseWriter, r *http.Request, u *store.User) error

// api wraps an API handler. scopes lists what an access token needs for the
// route ("code:read issues:write", "admin"); "" is any token.
func (s *Server) api(scopes string, fn apiFunc) http.HandlerFunc {
	type need struct {
		scope string
		lvl   store.Access
	}
	var needs []need
	for _, f := range strings.Fields(scopes) {
		name, lvl, _ := strings.Cut(f, ":")
		a, err := store.ParseAccess(lvl)
		if lvl == "" {
			a, err = store.AccessAdmin, nil
		}
		if err != nil {
			panic("bad route scope " + f)
		}
		needs = append(needs, need{name, a})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		pr, err := s.authenticate(r)
		if err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errUnauthorized, err))
			return
		}
		if pr.jobID != 0 {
			writeErr(w, forge.ErrForbidden)
			return
		}
		if pr.user == nil && !s.F.Cfg.AnonymousRead {
			w.Header().Set("WWW-Authenticate", `Basic realm="forge"`)
			writeErr(w, errUnauthorized)
			return
		}
		if pr.user != nil {
			for _, n := range needs {
				if err := pr.user.Grant.Require(n.scope, n.lvl); err != nil {
					writeErr(w, err)
					return
				}
			}
		}
		if err := fn(w, r, pr.user); err != nil {
			writeErr(w, err)
		}
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// logWriter sends each line written by git http-backend (and net/http/cgi's
// own messages) to the structured log.
type logWriter struct {
	log   zerolog.Logger
	level zerolog.Level
}

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.log.WithLevel(w.level).Str("component", "git-http").Msg(line)
		}
	}
	return len(p), nil
}

// cutWiki splits "owner/name.wiki" (the wiki repository) into "owner/name".
func cutWiki(full string) (string, bool) {
	full = strings.TrimSuffix(full, ".git")
	if len(full) > 5 && strings.EqualFold(full[len(full)-5:], ".wiki") {
		return full[:len(full)-5], true
	}
	return full, false
}

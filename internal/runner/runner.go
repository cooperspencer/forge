// Package runner is the CI execution agent. It pulls jobs from a forge
// server over HTTP, executes them in Docker (or directly on the host with the
// shell executor), streams logs back and uploads artifacts. Runners are
// stateless and can be added or removed at any time.
package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/rs/zerolog"
)

type Config struct {
	Server      string
	Token       string
	Labels      []string
	Concurrency int
	WorkDir     string
	Executor    string // docker | shell
	Docker      string // docker binary (docker or podman)
	// Registry overrides FORGE_REGISTRY for jobs (host[:port]) when the
	// --server address is not how job containers' Docker daemon reaches the
	// registry, e.g. a compose service name.
	Registry string
	// DockerSocket, if set, is the host's Docker socket mounted into job
	// containers at /var/run/docker.sock so jobs can build and push images.
	// Jobs then control the host's Docker daemon (effectively root on this
	// machine); never mounted for pull requests from forks.
	DockerSocket string
	// Network is the Docker network job containers join (docker executor),
	// e.g. the compose network so jobs reach the server by service name.
	Network string
	Version string
}

type Runner struct {
	cfg  Config
	http *http.Client
	log  zerolog.Logger
}

func New(cfg Config, log zerolog.Logger) (*Runner, error) {
	if cfg.Server == "" || cfg.Token == "" {
		return nil, errors.New("--server and --token are required")
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.Executor == "" {
		cfg.Executor = "docker"
	}
	if cfg.Docker == "" {
		cfg.Docker = "docker"
		if _, err := exec.LookPath("docker"); err != nil {
			if _, err := exec.LookPath("podman"); err == nil {
				cfg.Docker = "podman"
			}
		}
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = filepath.Join(os.TempDir(), "forge-runner")
	}
	// The executor, OS and architecture are always labels; --labels adds to them.
	seen := map[string]bool{}
	var labels []string
	base := []string{cfg.Executor, runtime.GOOS, runtime.GOARCH}
	if cfg.DockerSocket != "" {
		if cfg.Executor != "docker" {
			return nil, fmt.Errorf("--docker-socket needs the docker executor")
		}
		if _, err := os.Stat(cfg.DockerSocket); err != nil {
			return nil, fmt.Errorf("docker socket: %w", err)
		}
		base = append(base, "docker-socket") // pipelines can ask for it: runs-on: docker-socket
	}
	for _, l := range append(base, cfg.Labels...) {
		if l = strings.TrimSpace(l); l != "" && !seen[l] {
			seen[l] = true
			labels = append(labels, l)
		}
	}
	cfg.Labels = labels
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	if cfg.Executor == "docker" {
		if out, err := exec.Command(cfg.Docker, "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%s is not usable: %v: %s", cfg.Docker, err, strings.TrimSpace(string(out)))
		}
	}
	return &Runner{cfg: cfg, http: &http.Client{Timeout: 60 * time.Second}, log: log}, nil
}

// Run claims and executes jobs until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info().Str("server", r.cfg.Server).Str("labels", strings.Join(r.cfg.Labels, ",")).Str("executor", r.cfg.Executor).Int("concurrency", r.cfg.Concurrency).Msg("runner started")
	sem := make(chan struct{}, r.cfg.Concurrency)
	var wg sync.WaitGroup
	backoff := time.Second
	for ctx.Err() == nil {
		sem <- struct{}{}
		job, err := r.claim(ctx)
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				break
			}
			r.log.Warn().Err(err).Dur("retry_in_ms", backoff).Msg("claim failed")
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if job == nil {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.execute(ctx, job)
		}()
	}
	r.log.Info().Msg("shutting down; waiting for running jobs")
	wg.Wait()
	return nil
}

// ---------------------------------------------------------------- protocol

func (r *Runner) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.Server+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode == 200 {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (r *Runner) postJSON(ctx context.Context, path string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	return r.do(ctx, "POST", path, bytes.NewReader(b), "application/json", out)
}

func (r *Runner) claim(ctx context.Context) (*ci.JobPayload, error) {
	var p ci.JobPayload
	code, err := r.postJSON(ctx, "/api/v1/runner/claim", map[string]any{"labels": r.cfg.Labels, "version": r.cfg.Version, "wait": true}, &p)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNoContent {
		return nil, nil
	}
	return &p, nil
}

// ---------------------------------------------------------------- log shipping

// logShipper batches output and POSTs it to the server, masking secrets.
type logShipper struct {
	r     *Runner
	jobID int64
	mu    sync.Mutex
	buf   bytes.Buffer
	mask  []string
	done  chan struct{}
	wg    sync.WaitGroup
}

func newLogShipper(r *Runner, jobID int64, secrets map[string]string) *logShipper {
	l := &logShipper{r: r, jobID: jobID, done: make(chan struct{})}
	for _, v := range secrets {
		if len(v) >= 3 {
			l.mask = append(l.mask, v)
		}
	}
	// longest first so overlapping secrets are fully masked
	sort.Slice(l.mask, func(i, j int) bool { return len(l.mask[i]) > len(l.mask[j]) })
	l.wg.Add(1)
	go l.loop()
	return l
}

func (l *logShipper) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf.Write(p)
	big := l.buf.Len() > 64<<10
	l.mu.Unlock()
	if big {
		l.flush()
	}
	return len(p), nil
}

func (l *logShipper) Printf(format string, a ...any) { fmt.Fprintf(l, format, a...) }

func (l *logShipper) flush() {
	l.mu.Lock()
	if l.buf.Len() == 0 {
		l.mu.Unlock()
		return
	}
	data := l.buf.String()
	l.buf.Reset()
	l.mu.Unlock()
	for _, m := range l.mask {
		data = strings.ReplaceAll(data, m, "***")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := l.r.do(ctx, "POST", fmt.Sprintf("/api/v1/runner/jobs/%d/log", l.jobID), strings.NewReader(data), "text/plain", nil); err != nil {
		l.r.log.Warn().Err(err).Int64("job", l.jobID).Msg("log upload failed")
	}
}

func (l *logShipper) loop() {
	defer l.wg.Done()
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.flush()
		case <-l.done:
			l.flush()
			return
		}
	}
}

func (l *logShipper) Close() { close(l.done); l.wg.Wait() }

// ---------------------------------------------------------------- execution

const (
	cDim    = "\x1b[2m"
	cBold   = "\x1b[1m"
	cCyan   = "\x1b[36m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cMag    = "\x1b[35m"
	cYellow = "\x1b[33m"
	cReset  = "\x1b[0m"
)

func (r *Runner) execute(parent context.Context, p *ci.JobPayload) {
	log := r.log.With().Int64("job", p.JobID).Str("repo", p.Repo).Str("name", p.Spec.Name).Logger()
	log.Info().Msg("job started")
	// The job token lets steps use the forge as the job (clone, push packages
	// of this repository); mask it like a secret.
	masked := map[string]string{"FORGE_TOKEN": p.CloneToken}
	for k, v := range p.Secrets {
		masked[k] = v
	}
	out := newLogShipper(r, p.JobID, masked)
	timeout := p.Spec.Timeout
	if timeout <= 0 {
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// heartbeat + remote cancellation
	var cancelled, lost bool
	var flagMu sync.Mutex
	hbDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		fails := 0
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				var resp struct{ Cancel bool }
				hctx, hc := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := r.postJSON(hctx, fmt.Sprintf("/api/v1/runner/jobs/%d/heartbeat", p.JobID), nil, &resp)
				hc()
				if err != nil {
					fails++
					if fails >= 12 { // ~1 min without contact
						flagMu.Lock()
						lost = true
						flagMu.Unlock()
						cancel()
					}
					continue
				}
				fails = 0
				if resp.Cancel {
					flagMu.Lock()
					cancelled = true
					flagMu.Unlock()
					cancel()
				}
			}
		}
	}()

	status, exit := r.runJob(ctx, p, out)
	close(hbDone)
	flagMu.Lock()
	switch {
	case cancelled:
		status = "cancelled"
		out.Printf("\n%s[forge] job cancelled%s\n", cRed, cReset)
	case lost:
		status = "error"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status = "failure"
		out.Printf("\n%s[forge] job exceeded its timeout of %s%s\n", cRed, timeout, cReset)
	case parent.Err() != nil:
		status = "error"
		out.Printf("\n%s[forge] runner shut down while the job was running%s\n", cRed, cReset)
	}
	flagMu.Unlock()
	color := cGreen
	if status != "success" {
		color = cRed
	}
	out.Printf("\n%s%s[forge] job finished: %s%s\n", cBold, color, status, cReset)
	out.Close()

	fctx, fc := context.WithTimeout(context.Background(), 30*time.Second)
	defer fc()
	if _, err := r.postJSON(fctx, fmt.Sprintf("/api/v1/runner/jobs/%d/finish", p.JobID), map[string]any{"status": status, "exit_code": exit}, nil); err != nil {
		log.Error().Err(err).Msg("report finish")
	}
	log.Info().Str("status", status).Int("exit", exit).Msg("job finished")
}

// runJob prepares the workspace, runs every step and collects artifacts.
func (r *Runner) runJob(ctx context.Context, p *ci.JobPayload, out *logShipper) (status string, exit int) {
	ws := filepath.Join(r.cfg.WorkDir, fmt.Sprintf("job-%d", p.JobID))
	os.RemoveAll(ws)
	if err := os.MkdirAll(ws, 0o777); err != nil {
		out.Printf("%s[forge] cannot create workspace: %v%s\n", cRed, err, cReset)
		return "error", -1
	}
	defer os.RemoveAll(ws)

	out.Printf("%s%s▸ checkout%s %s%s @ %s%s\n", cBold, cCyan, cReset, cDim, p.Repo, short(p.SHA), cReset)
	if err := r.checkout(ctx, p, ws, out); err != nil {
		out.Printf("%s[forge] checkout failed: %v%s\n", cRed, err, cReset)
		return "error", -1
	}

	env := map[string]string{}
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range p.Spec.Env {
		env[k] = v
	}
	for k, v := range p.Secrets {
		env[k] = v
	}
	env["FORGE_TOKEN"] = p.CloneToken
	if env["FORGE_REGISTRY"] != "" && r.registryHost() != "" {
		env["FORGE_REGISTRY"] = r.registryHost()
	}

	if r.cfg.Executor != "shell" && !ci.ValidImage(p.Spec.Image) {
		out.Printf("%s[forge] invalid image %q%s\n", cRed, p.Spec.Image, cReset)
		return "error", -1
	}
	var ex executor
	switch r.cfg.Executor {
	case "shell":
		ex = &shellExecutor{dir: ws, env: env}
	default:
		d := &dockerExecutor{bin: r.cfg.Docker, name: fmt.Sprintf("forge-job-%d-%d", p.JobID, time.Now().Unix()), image: p.Spec.Image, ws: ws, env: env, network: r.cfg.Network}
		if r.cfg.DockerSocket != "" {
			if p.Spec.Fork {
				out.Printf("%s[forge] pull request from a fork: the Docker socket is not available%s\n", cDim, cReset)
			} else {
				d.socket = r.cfg.DockerSocket
				out.Printf("%s[forge] the host's Docker daemon is available at /var/run/docker.sock%s\n", cDim, cReset)
			}
		}
		ex = d
	}
	out.Printf("%s%s▸ prepare%s %s%s executor, image %s%s\n", cBold, cCyan, cReset, cDim, r.cfg.Executor, p.Spec.Image, cReset)
	if err := ex.Start(ctx, out); err != nil {
		out.Printf("%s[forge] could not start environment: %v%s\n", cRed, err, cReset)
		ex.Stop()
		return "error", -1
	}
	defer ex.Stop()

	status = "success"
	for i, step := range p.Spec.Steps {
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		out.Printf("\n%s%s▸ %s%s\n", cBold, cCyan, name, cReset)
		for _, l := range strings.Split(strings.TrimSpace(step.Run), "\n") {
			out.Printf("%s%s$%s %s%s%s\n", cBold, cMag, cReset, cDim, l, cReset)
		}
		start := time.Now()
		code, err := ex.Exec(ctx, step.Run, out)
		took := time.Since(start).Round(100 * time.Millisecond)
		if err != nil && ctx.Err() == nil {
			out.Printf("%s[forge] step error: %v%s\n", cRed, err, cReset)
			status, exit = "error", -1
			break
		}
		if ctx.Err() != nil {
			status, exit = "failure", -1
			break
		}
		if code != 0 {
			out.Printf("%s%s✗ %s failed with exit code %d%s %s(%s)%s\n", cBold, cRed, name, code, cReset, cDim, took, cReset)
			status, exit = "failure", code
			break
		}
		out.Printf("%s✓%s %s %s(%s)%s\n", cGreen, cReset, name, cDim, took, cReset)
	}

	if len(p.Spec.Artifacts) > 0 && ctx.Err() == nil {
		out.Printf("\n%s%s▸ artifacts%s\n", cBold, cCyan, cReset)
		ex.Fix(ctx) // make container-created files readable
		r.uploadArtifacts(ctx, p, ws, out)
	}
	return status, exit
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (r *Runner) checkout(ctx context.Context, p *ci.JobPayload, ws string, out io.Writer) error {
	auth := base64.StdEncoding.EncodeToString([]byte(p.CloneUser + ":" + p.CloneToken))
	git := func(args ...string) error {
		c := exec.CommandContext(ctx, "git", append([]string{"-c", "http.extraHeader=Authorization: Basic " + auth}, args...)...)
		c.Dir = ws
		// LFS files are fetched explicitly below, with a log line
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
		var stderr bytes.Buffer
		c.Stdout, c.Stderr = io.Discard, &stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", r.cloneURL(p)},
		{"fetch", "-q", "--depth=50", "origin", p.SHA},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if err := git(s...); err != nil {
			// fall back to fetching the ref if the server refuses SHA wants
			if s[0] == "fetch" {
				if err2 := git("fetch", "-q", "origin", "+"+p.Ref+":refs/remotes/origin/ci"); err2 == nil {
					if err3 := git("checkout", "-q", "--detach", p.SHA); err3 == nil {
						break
					}
				}
			}
			return err
		}
	}
	if fi, err := os.Stat(filepath.Join(ws, ".gitmodules")); err == nil && !fi.IsDir() {
		git("submodule", "update", "--init", "--recursive", "--depth=1")
	}
	if p.LFS || usesLFS(ws) {
		if _, err := exec.LookPath("git-lfs"); err != nil {
			fmt.Fprintf(out, "%s[forge] this repository uses Git LFS, but git-lfs is not installed on the runner: LFS files stay pointer files%s\n", cYellow, cReset)
		} else if err := git("lfs", "pull"); err != nil {
			return fmt.Errorf("fetching Git LFS files: %w", err)
		} else {
			fmt.Fprintf(out, "%sGit LFS files fetched%s\n", cDim, cReset)
		}
	}
	fmt.Fprintf(out, "%scheckout complete%s\n", cDim, cReset)
	return nil
}

// uploadArtifacts globs patterns relative to the workspace. Directories are
// uploaded as .tar.gz archives.
func (r *Runner) uploadArtifacts(ctx context.Context, p *ci.JobPayload, ws string, out *logShipper) {
	seen := map[string]bool{}
	for _, pat := range p.Spec.Artifacts {
		matches, _ := filepath.Glob(filepath.Join(ws, filepath.Clean("/" + pat)[1:]))
		if len(matches) == 0 {
			out.Printf("%sno files match %q%s\n", cDim, pat, cReset)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			var body io.Reader
			name := filepath.Base(m)
			if fi.IsDir() {
				var buf bytes.Buffer
				if err := tarDir(&buf, m); err != nil {
					out.Printf("%sfailed to archive %s: %v%s\n", cRed, name, err, cReset)
					continue
				}
				body, name = &buf, name+".tar.gz"
			} else {
				f, err := os.Open(m)
				if err != nil {
					out.Printf("%scannot read %s: %v%s\n", cRed, name, err, cReset)
					continue
				}
				defer f.Close()
				body = f
			}
			path := fmt.Sprintf("/api/v1/runner/jobs/%d/artifacts?name=%s", p.JobID, urlQueryEscape(name))
			if _, err := r.do(ctx, "POST", path, body, "application/octet-stream", nil); err != nil {
				out.Printf("%supload of %s failed: %v%s\n", cRed, name, err, cReset)
				continue
			}
			out.Printf("uploaded %s\n", name)
		}
	}
}

func tarDir(w io.Writer, dir string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base := filepath.Dir(dir)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// cloneURL is the job's repository under the --server URL this runner already
// reaches the server with. The server's own public_url may not be reachable
// from here (e.g. it defaults to localhost, or the runner is on a LAN/VPN
// address), and the job token authenticates either way.
func (r *Runner) cloneURL(p *ci.JobPayload) string {
	if p.Repo != "" {
		return r.cfg.Server + "/" + p.Repo + ".git"
	}
	return p.CloneURL
}

// registryHost is the container registry as reachable from this runner: the
// --server address without scheme (like cloneURL, the server's own public
// URL may not be reachable from here).
func (r *Runner) registryHost() string {
	if r.cfg.Registry != "" {
		return r.cfg.Registry
	}
	h := r.cfg.Server
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	return strings.TrimRight(h, "/")
}

// usesLFS reports whether the checkout's .gitattributes routes files through
// the LFS filter.
func usesLFS(ws string) bool {
	b, err := os.ReadFile(filepath.Join(ws, ".gitattributes"))
	return err == nil && bytes.Contains(b, []byte("filter=lfs"))
}

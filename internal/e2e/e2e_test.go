package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/rs/zerolog"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/config"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/logging"
	"github.com/cooperspencer/forge/internal/runner"
	"github.com/cooperspencer/forge/internal/store"
	"github.com/cooperspencer/forge/internal/web"
)

type env struct {
	t     *testing.T
	f     *forge.Forge
	srv   *httptest.Server
	token string
	admin *store.User
}

func setup(t *testing.T) *env {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.CI.StaleSeconds = 30
	log := zerolog.Nop()
	if testing.Verbose() {
		log = logging.New(logging.Options{Level: "debug"})
	}
	f, err := forge.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(f).Handler())
	cfg.HTTP.PublicURL = srv.URL
	t.Cleanup(func() { srv.Close(); f.Close() })
	ctx := context.Background()
	u, err := f.Store.CreateUser(ctx, "admin", true, false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := f.Store.CreateAccessToken(ctx, u.ID, "test")
	return &env{t: t, f: f, srv: srv, token: tok, admin: u}
}

func (e *env) api(method, path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	if resp.StatusCode >= 400 {
		e.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x", "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) startRunner(t *testing.T, labels ...string) context.CancelFunc {
	tok, err := e.f.Store.CreateRunner(context.Background(), fmt.Sprintf("r%d", time.Now().UnixNano()%100000), labels)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{Server: e.srv.URL, Token: tok, Executor: "shell", Concurrency: 3, WorkDir: t.TempDir(), Labels: labels},
		zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

type runResp struct {
	Number  int64  `json:"number"`
	Status  string `json:"status"`
	Trigger string `json:"trigger"`
	Ref     string `json:"ref"`
	Error   string `json:"error"`
	Jobs    []struct {
		ID     int64  `json:"id"`
		Key    string `json:"key"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"jobs"`
}

func (e *env) waitRun(repo string, n int64, want ...string) runResp {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var r runResp
	for time.Now().Before(deadline) {
		e.api("GET", fmt.Sprintf("/api/v1/repos/%s/runs/%d", repo, n), nil, &r)
		if store.IsTerminal(r.Status) || slices.Contains(want, r.Status) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ok := false
	for _, w := range want {
		if r.Status == w {
			ok = true
		}
	}
	if !ok {
		e.t.Fatalf("run #%d: status %s, want %v; %+v", n, r.Status, want, r)
	}
	return r
}

const pipeline = `name: ci
on:
  push: {}
  pull_request: {}
  manual: true
env:
  GREETING: hello
jobs:
  build:
    image: alpine:3
    steps:
      - name: build
        run: |
          mkdir -p out
          echo "$GREETING from $FORGE_REPO@$FORGE_SHA" > out/result.txt
          echo built
    artifacts: [out/result.txt, out]
  test:
    image: alpine:3
    needs: build
    matrix:
      n: ["1", "2"]
    secrets: [TOKEN, owner_wide]
    steps:
      - echo "matrix ${{ matrix.n }} env $MATRIX_N"
      - test "$TOKEN" = "s3cr3t-value" && echo "secret is $TOKEN"
      - test "$OWNER_WIDE" = "inherited-value" && echo "inherited secret ok"
`

func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	// The runner must work through its --server URL even when the server's
	// advertised public URL is unreachable from the runner's machine.
	e.f.Cfg.HTTP.PublicURL = "http://public-url.invalid"

	// create repo through the API
	var repo map[string]any
	if c := e.api("POST", "/api/v1/repos", map[string]any{"name": "demo", "description": "e2e"}, &repo); c != 201 {
		t.Fatalf("create repo: %d", c)
	}
	// Owner-level secrets are inherited; a repository secret of the same name wins.
	for path, val := range map[string]string{
		"/api/v1/users/admin/secrets/TOKEN":      "owner-value-overridden",
		"/api/v1/users/admin/secrets/OWNER_WIDE": "inherited-value",
		"/api/v1/repos/admin/demo/secrets/token": "s3cr3t-value",
	} {
		if c := e.api("PUT", path, map[string]any{"value": val}, nil); c != 204 {
			t.Fatalf("PUT %s: %d", path, c)
		}
	}
	if c := e.api("PUT", "/api/v1/repos/admin/demo/secrets/FORGE_X", map[string]any{"value": "x"}, nil); c != 400 {
		t.Errorf("reserved secret name accepted: %d", c)
	}
	var listed []map[string]any
	e.api("GET", "/api/v1/repos/admin/demo/secrets", nil, &listed)
	if len(listed) != 3 || listed[0]["name"] != "TOKEN" || listed[1]["overridden"] != false || listed[2]["overridden"] != true {
		t.Errorf("repo secret listing: %+v", listed)
	}

	e.startRunner(t, "shell")

	// push a commit over smart HTTP using the token as password
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, ".forge.yml"), []byte(pipeline), 0o644)
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# demo\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "initial")
	remote := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/demo.git"
	git(t, work, "remote", "add", "origin", remote)
	git(t, work, "push", "-q", "origin", "main")

	run := e.waitRun("admin/demo", 1, store.StatusSuccess)
	if len(run.Jobs) != 3 {
		t.Fatalf("expected 3 jobs (build + 2 matrix), got %+v", run.Jobs)
	}
	// logs: matrix value, secret masked
	var logs string
	for _, j := range run.Jobs {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/repos/admin/demo/jobs/%d/log", e.srv.URL, j.ID), nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		logs += string(b)
	}
	for _, want := range []string{"built", "matrix 1 env 1", "matrix 2 env 2", "secret is ***", "inherited secret ok"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "s3cr3t-value") {
		t.Error("secret leaked into logs")
	}

	// artifacts
	var arts []map[string]any
	e.api("GET", "/api/v1/repos/admin/demo/runs/1/artifacts", nil, &arts)
	if len(arts) != 2 {
		t.Fatalf("artifacts: %+v", arts)
	}
	a, _ := e.f.Store.ArtifactByID(ctx, int64(arts[0]["id"].(float64)))
	data, _ := os.ReadFile(a.Path)
	if !strings.Contains(string(data), "hello from admin/demo@") {
		t.Errorf("artifact content %q", data)
	}

	// feature branch + pull request
	git(t, work, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(work, "feature.txt"), []byte("new\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "add feature")
	git(t, work, "push", "-q", "origin", "feature")
	e.waitRun("admin/demo", 2, store.StatusSuccess) // push run

	var pr map[string]any
	if c := e.api("POST", "/api/v1/repos/admin/demo/pulls", map[string]any{"head": "feature", "base": "main", "body": "please merge"}, &pr); c != 201 {
		t.Fatalf("create PR: %d %v", c, pr)
	}
	if pr["title"] != "add feature" {
		t.Errorf("PR title defaulted to %v", pr["title"])
	}
	prRun := e.waitRun("admin/demo", 3, store.StatusSuccess)
	if prRun.Trigger != "pull_request" || prRun.Ref != "refs/pull/1/head" {
		t.Errorf("PR run: %+v", prRun)
	}
	var st map[string]any
	e.api("GET", "/api/v1/repos/admin/demo/pulls/1", nil, &st)
	if st["mergeable"] != true || st["checks_passed"] != true {
		t.Errorf("PR status: %v", st)
	}
	// refs/pull is read-only for clients
	c := exec.Command("git", "push", "origin", "HEAD:refs/pull/1/head")
	c.Dir = work
	if out, err := c.CombinedOutput(); err == nil {
		t.Errorf("push to refs/pull should be refused: %s", out)
	}

	var merged map[string]string
	if c := e.api("POST", "/api/v1/repos/admin/demo/pulls/1/merge", map[string]string{"method": "merge"}, &merged); c != 200 {
		t.Fatalf("merge: %d %v", c, merged)
	}
	r, _ := e.f.Store.RepoByName(ctx, "admin", "demo")
	head, _ := e.f.Git(r).ResolveRef(ctx, "refs/heads/main")
	if head != merged["sha"] {
		t.Errorf("main=%s merge=%s", head, merged["sha"])
	}
	cm, _ := e.f.Git(r).Commit(ctx, head)
	if len(cm.Parents) != 2 {
		t.Errorf("expected merge commit, parents=%v", cm.Parents)
	}
	i, _ := e.f.Store.IssueByNumber(ctx, r.ID, 1)
	if i.State != "merged" {
		t.Errorf("PR state %s", i.State)
	}
	e.waitRun("admin/demo", 4, store.StatusSuccess) // merge triggers a push run on main

	// HTTP clone with a fresh checkout contains the merged file
	clone := t.TempDir()
	git(t, clone, "clone", "-q", remote, ".")
	if _, err := os.Stat(filepath.Join(clone, "feature.txt")); err != nil {
		t.Error("merged file missing in clone")
	}

	// anonymous clone of a private repo is refused
	e.api("PATCH", "/api/v1/repos/admin/demo", map[string]any{"private": true}, nil)
	anon := exec.Command("git", "clone", "-q", e.srv.URL+"/admin/demo.git", t.TempDir())
	anon.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := anon.CombinedOutput(); err == nil {
		t.Errorf("anonymous clone of private repo succeeded: %s", out)
	}
}

func TestCancelAndRerun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	e.api("POST", "/api/v1/repos", map[string]any{"name": "slow"}, nil)
	e.startRunner(t, "shell")
	r, _ := e.f.Store.RepoByName(ctx, "admin", "slow")

	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(work, ".forge/pipelines"), 0o755)
	os.WriteFile(filepath.Join(work, ".forge/pipelines/slow.yml"), []byte(`
on: {manual: true}
jobs:
  wait:
    image: alpine
    steps: ["echo started; sleep 30"]
  after:
    image: alpine
    needs: [wait]
    steps: ["echo never"]
`), 0o644)
	os.WriteFile(filepath.Join(work, ".forge/pipelines/broken.yml"), []byte("jobs:\n  x:\n    steps: [echo]\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "slow")
	remote := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/slow.git"
	git(t, work, "push", "-q", remote, "main")

	// the push creates only the broken pipeline's error run (slow is manual-only)
	runs, _ := e.f.Store.Runs(ctx, r.ID, 10)
	if len(runs) != 1 || runs[0].Status != store.StatusError || !strings.Contains(runs[0].Error, "image is required") {
		t.Fatalf("expected one errored run for broken.yml, got %+v", runs)
	}

	var started []runResp
	if c := e.api("POST", "/api/v1/repos/admin/slow/runs", map[string]any{"pipeline": "slow"}, &started); c != 201 || len(started) != 1 {
		t.Fatalf("trigger: %d %+v", c, started)
	}
	n := started[0].Number
	// wait until running
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var rr runResp
		e.api("GET", fmt.Sprintf("/api/v1/repos/admin/slow/runs/%d", n), nil, &rr)
		if rr.Status == store.StatusRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	e.api("POST", fmt.Sprintf("/api/v1/repos/admin/slow/runs/%d/cancel", n), nil, nil)
	res := e.waitRun("admin/slow", n, store.StatusCancelled)
	for _, j := range res.Jobs {
		if j.Status != store.StatusCancelled {
			t.Errorf("job %s: %s", j.Name, j.Status)
		}
	}
	if c := e.api("POST", fmt.Sprintf("/api/v1/repos/admin/slow/runs/%d/rerun", n), nil, nil); c != 202 {
		t.Fatalf("rerun: %d", c)
	}
	var rr runResp
	e.api("GET", fmt.Sprintf("/api/v1/repos/admin/slow/runs/%d", n), nil, &rr)
	if store.IsTerminal(rr.Status) {
		t.Errorf("rerun did not reset run: %+v", rr)
	}
	e.api("POST", fmt.Sprintf("/api/v1/repos/admin/slow/runs/%d/cancel", n), nil, nil)
	e.waitRun("admin/slow", n, store.StatusCancelled)
}

func TestPermissions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	carol, _ := f.Store.CreateUser(ctx, "carol", false, false)
	r, err := f.CreateRepo(ctx, bob, "", "secret", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.ResolveRepo(ctx, carol, "bob/secret", store.AccessRead); err != store.ErrNotFound {
		t.Errorf("carol should not see private repo: %v", err)
	}
	f.Store.SetCollaborator(ctx, r.ID, carol.ID, "read")
	if _, _, err := f.ResolveRepo(ctx, carol, "bob/secret", store.AccessRead); err != nil {
		t.Errorf("collaborator read: %v", err)
	}
	if _, _, err := f.ResolveRepo(ctx, carol, "bob/secret", store.AccessWrite); err != forge.ErrForbidden {
		t.Errorf("read collaborator must not write: %v", err)
	}
	if _, err := f.CreateRepo(ctx, carol, "bob", "x", "", false); err != forge.ErrForbidden {
		t.Errorf("carol must not create in bob's namespace: %v", err)
	}
	org, _ := f.CreateOrg(ctx, bob, "acme")
	f.Store.SetOrgMember(ctx, org.ID, carol.ID, "member")
	or, err := f.CreateRepo(ctx, carol, "acme", "tool", "", true)
	if err != nil {
		t.Fatalf("org member create: %v", err)
	}
	if acc := f.Store.AccessLevel(ctx, bob, or); acc != store.AccessAdmin {
		t.Errorf("org owner access = %v", acc)
	}
	if acc := f.Store.AccessLevel(ctx, carol, or); acc != store.AccessWrite {
		t.Errorf("org member access = %v", acc)
	}
	if err := f.DeleteRepo(ctx, carol, or); err != forge.ErrForbidden {
		t.Errorf("member delete: %v", err)
	}
	// self-review is rejected
	i, _ := f.Store.CreatePullRequest(ctx, r.ID, bob.ID, "x", "", store.PullRequest{HeadRepoID: r.ID, HeadBranch: "a", BaseBranch: "main"})
	if err := f.Comment(ctx, bob, r, i, &store.Comment{Review: "approve"}); err == nil {
		t.Error("author approved own PR")
	}
	// job tokens
	tok := f.JobToken(42)
	if id, ok := f.VerifyJobToken(tok); !ok || id != 42 {
		t.Error("job token roundtrip")
	}
	if _, ok := f.VerifyJobToken("42.deadbeef"); ok {
		t.Error("forged job token accepted")
	}
}

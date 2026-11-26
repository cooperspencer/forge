package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/sshd"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// TestMirrors: pull mirrors follow a remote (and are read-only), push mirrors
// copy a repository out after pushes; over HTTP with a token and over SSH
// with a generated deploy key. The "remote" is this same forge.
func TestMirrors(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	f.Cfg.Mirror.AllowLoopback = true
	go f.MirrorLoop(ctx)

	// the original: a private repository with two branches and a tag
	src, _ := f.CreateRepo(ctx, e.admin, "admin", "upstream", "", true)
	work := t.TempDir()
	commit := func(msg string) {
		t.Helper()
		os.WriteFile(filepath.Join(work, "f.txt"), []byte(msg), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", msg)
	}
	git(t, work, "init", "-q", "-b", "trunk")
	os.WriteFile(filepath.Join(work, ".forge.yml"), []byte("name: ci\non: {push: {}}\njobs:\n  t: {image: alpine, steps: [\"true\"]}\n"), 0o644)
	commit("one")
	git(t, work, "tag", "v1")
	e.pushAs(t, work, "admin", e.token, "admin/upstream", "trunk")
	git(t, work, "push", "-q", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/upstream.git", "v1", "trunk:feature")
	refs := func(r *store.Repo) map[string]string {
		m, _ := f.Git(r).RefMap(ctx)
		return m
	}
	same := func(a, b *store.Repo) bool { return fmt.Sprint(refs(a)) == fmt.Sprint(refs(b)) }

	// --- pull mirror over HTTP with a token
	cp, err := f.CreatePullMirror(ctx, e.admin, "admin", "copy", "", false, forge.MirrorOptions{
		URL: e.srv.URL + "/admin/upstream.git", Username: "admin", Password: e.token, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Name != "copy" || !cp.Mirror {
		t.Errorf("mirror repo: %+v", cp)
	}
	waitFor(t, "first pull sync", func() bool { return same(src, cp) })
	var m *store.Mirror
	waitFor(t, "the sync result", func() bool { m, _ = f.Store.PullMirror(ctx, cp.ID); return !m.LastSync.IsZero() })
	cp, _ = f.Store.RepoByID(ctx, cp.ID)
	if cp.DefaultBranch != "trunk" {
		t.Errorf("default branch follows the remote: %q", cp.DefaultBranch)
	}
	if m.Secret == "" || strings.Contains(m.Secret, e.token) || m.LastError != "" || m.LastSync.IsZero() || m.NextSync.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("mirror state: %+v", m)
	}

	// changes upstream arrive on the next sync, deletions included
	commit("two")
	e.pushAs(t, work, "admin", e.token, "admin/upstream", "trunk")
	git(t, work, "push", "-q", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/upstream.git", ":feature")
	if err := f.SyncMirror(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if !same(src, cp) {
		t.Errorf("after sync:\n%v\n%v", refs(src), refs(cp))
	}
	var evs []struct{ Summary string }
	if e.api("GET", "/api/v1/repos/admin/"+cp.Name+"/events", nil, &evs); !strings.Contains(fmt.Sprint(evs), "mirror pushed 1 commit(s) to trunk") {
		t.Errorf("events: %v", evs)
	}
	// mirrored commits don't run pipelines (the original's push did)
	var cpRuns []any
	e.api("GET", "/api/v1/repos/admin/"+cp.Name+"/runs", nil, &cpRuns)
	if len(cpRuns) != 0 {
		t.Errorf("a mirror sync ran pipelines: %v", cpRuns)
	}

	// read-only
	if out, err := runGit(work, "push", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/"+cp.Name+".git", "trunk:other"); err == nil || !strings.Contains(out, "read-only mirror") {
		t.Errorf("push to a pull mirror: %v\n%s", err, out)
	}
	if err := f.DeleteBranch(ctx, e.admin, cp, "nope"); err == nil {
		t.Error("deleted a branch of a mirror")
	}
	if _, err := f.CreateRelease(ctx, e.admin, cp, "v9", "trunk", "", ""); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("new tag in a mirror: %v", err)
	}

	// a wrong token is reported, and the mirror keeps its last good state
	f.UpdateMirror(ctx, e.admin, cp, m.ID, forge.MirrorOptions{URL: m.URL, Username: "admin", Password: "fgp_wrong", Interval: time.Hour})
	if err := f.SyncMirror(ctx, m.ID); err == nil {
		t.Error("sync with a wrong token succeeded")
	}
	m2, _ := f.Store.MirrorByID(ctx, m.ID)
	if m2.LastError == "" || !m2.LastSync.Equal(m.LastSync) && m2.LastSync.Before(m.LastSync) {
		t.Errorf("failed sync state: %+v", m2)
	}
	if strings.Contains(m2.LastError, "fgp_wrong") {
		t.Errorf("error leaks the token: %s", m2.LastError)
	}

	// stopping the mirror makes the repository writable
	if err := f.RemoveMirror(ctx, e.admin, cp, m.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(work, "push", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/"+cp.Name+".git", "trunk:other"); err != nil {
		t.Errorf("push after stopping the mirror: %v\n%s", err, out)
	}

	// --- push mirror over HTTP: every push is copied out
	app, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	target, _ := f.CreateRepo(ctx, e.admin, "admin", "app-copy", "", true)
	if _, err := f.AddPushMirror(ctx, e.admin, app, forge.MirrorOptions{URL: e.srv.URL + "/admin/app-copy.git", Username: "admin", Password: e.token}); err != nil {
		t.Fatal(err)
	}
	e.pushAs(t, work, "admin", e.token, "admin/app", "trunk")
	waitFor(t, "push mirror after a push", func() bool { return refs(target)["refs/heads/trunk"] == refs(app)["refs/heads/trunk"] })
	commit("three")
	e.pushAs(t, work, "admin", e.token, "admin/app", "trunk")
	waitFor(t, "push mirror after the second push", func() bool { return refs(target)["refs/heads/trunk"] == refs(app)["refs/heads/trunk"] })
	if refs(app)["refs/pull/1/head"] != "" || len(refs(target)) != len(refs(app)) {
		t.Errorf("push mirror refs: %v vs %v", refs(target), refs(app))
	}

	// --- push mirror over SSH with a deploy key
	f.Cfg.SSH.Listen = freePort(t)
	srv, err := sshd.New(f)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	_, port, _ := net.SplitHostPort(f.Cfg.SSH.Listen)
	sshTarget, _ := f.CreateRepo(ctx, e.admin, "admin", "ssh-copy", "", true)
	pm, err := f.AddPushMirror(ctx, e.admin, app, forge.MirrorOptions{URL: "ssh://git@127.0.0.1:" + port + "/admin/ssh-copy.git"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pm.SSHPub, "ssh-ed25519 ") || pm.Secret != "" {
		t.Fatalf("deploy key: %+v", pm)
	}
	if err := f.SyncMirror(ctx, pm.ID); err == nil {
		t.Error("ssh push before the key was added should fail")
	}
	// "add the deploy key" on the remote: here, as one of admin's keys
	pk, _, _, _, _ := gossh.ParseAuthorizedKey([]byte(pm.SSHPub))
	if err := f.AddKey(ctx, e.admin, string(gossh.MarshalAuthorizedKey(pk))); err != nil {
		t.Fatal(err)
	}
	if err := f.SyncMirror(ctx, pm.ID); err != nil {
		t.Fatalf("ssh push mirror: %v", err)
	}
	if refs(sshTarget)["refs/heads/trunk"] != refs(app)["refs/heads/trunk"] {
		t.Error("ssh push mirror didn't copy trunk")
	}

	// --- remotes that would reach the server itself are refused
	f.Cfg.Mirror.AllowLoopback = false
	for _, bad := range []string{
		"file:///etc", "/var/lib/forge/repos/x.git", "ext::sh -c touch% /tmp/pwned", "-upload-pack=evil",
		"http://127.0.0.1/x.git", "https://localhost/x.git", "http://169.254.169.254/latest", "ftp://example.com/x.git", "https://example.com/",
	} {
		if _, err := f.ParseMirrorURL(ctx, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	rem, err := f.ParseMirrorURL(ctx, "https://bob:s3cret@example.com/x/y.git")
	if err != nil || rem.URL != "https://example.com/x/y.git" || rem.Username != "bob" || rem.Password != "s3cret" {
		t.Errorf("credentials in the URL: %v %+v", err, rem)
	}
	if rem, err := f.ParseMirrorURL(ctx, "git@example.com:x/y.git"); err != nil || !rem.SSH {
		t.Errorf("scp-like: %v %+v", err, rem)
	}
	// only admins manage mirrors
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	f.Store.SetCollaborator(ctx, app.ID, bob.ID, "write")
	if _, err := f.AddPushMirror(ctx, bob, app, forge.MirrorOptions{URL: "https://example.com/x.git"}); err == nil {
		t.Error("a writer added a push mirror")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runGit(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := c.CombinedOutput()
	return string(out), err
}

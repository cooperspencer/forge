package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/sshd"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// TestWiki: pages through the API, renames, concurrent edits, permissions,
// and the wiki as a git repository (clone, push, no CI), over HTTP and SSH.
func TestWiki(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	if e.api("POST", "/api/v1/repos", map[string]any{"name": "docs"}, nil) != 201 {
		t.Fatal("create repo")
	}
	type page struct {
		Name, Path, Author, Content string
		Head                        string `json:"wiki_head"`
	}
	names := func() string {
		t.Helper()
		var ps []page
		e.api("GET", "/api/v1/repos/admin/docs/wiki", nil, &ps)
		var n []string
		for _, p := range ps {
			n = append(n, p.Name)
		}
		return strings.Join(n, ", ")
	}
	if got := names(); got != "" {
		t.Errorf("new wiki: %q", got)
	}

	var p page
	if code := e.api("PUT", "/api/v1/repos/admin/docs/wiki/Home", map[string]any{"content": "# Welcome\n\nSee [[Getting Started]]."}, &p); code != 201 || p.Path != "Home.md" {
		t.Fatalf("create Home: %d %+v", code, p)
	}
	e.api("PUT", "/api/v1/repos/admin/docs/wiki/Getting%20Started", map[string]any{"content": "step one"}, nil)
	e.api("PUT", "/api/v1/repos/admin/docs/wiki/guides/Setup", map[string]any{"content": "setup"}, nil)
	if got := names(); got != "Home, Getting Started, guides/Setup" {
		t.Errorf("pages: %q", got)
	}
	e.api("GET", "/api/v1/repos/admin/docs/wiki/getting-started", nil, &p) // case and dash insensitive
	if p.Content != "step one\n" || p.Path != "Getting-Started.md" || p.Author != "admin" {
		t.Errorf("get page: %+v", p)
	}

	// edit and rename
	if code := e.api("PUT", "/api/v1/repos/admin/docs/wiki/Getting%20Started", map[string]any{"content": "step one\nstep two", "name": "Quick Start", "expect": p.Head}, &p); code != 200 || p.Name != "Quick Start" {
		t.Fatalf("rename: %d %+v", code, p)
	}
	if got := names(); got != "Home, guides/Setup, Quick Start" {
		t.Errorf("after rename: %q", got)
	}
	if code := e.api("PUT", "/api/v1/repos/admin/docs/wiki/Quick%20Start", map[string]any{"content": "x", "name": "Home"}, nil); code != 400 {
		t.Errorf("rename onto an existing page: %d", code)
	}
	// someone saved in between: the stale editor is refused
	stale := p.Head
	e.api("PUT", "/api/v1/repos/admin/docs/wiki/Home", map[string]any{"content": "# Welcome!"}, nil)
	if code := e.api("PUT", "/api/v1/repos/admin/docs/wiki/Quick%20Start", map[string]any{"content": "lost update", "expect": stale}, nil); code != 400 {
		t.Errorf("stale edit accepted: %d", code)
	}
	hist, err := f.WikiHistory(ctx, e.admin, mustRepo(t, f, "admin/docs"), "Home")
	if err != nil || len(hist) != 2 || hist[0].Subject != "Update Home" || hist[1].Subject != "Create Home" {
		t.Errorf("history: %v %+v", err, hist)
	}
	_, old, _ := f.WikiPageContent(ctx, e.admin, mustRepo(t, f, "admin/docs"), "Home", hist[1].SHA)
	if !strings.Contains(old, "See [[Getting Started]]") {
		t.Errorf("old version: %q", old)
	}

	// permissions
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	asBob := &env{t: t, f: f, srv: e.srv, token: bobTok}
	if code := asBob.api("GET", "/api/v1/repos/admin/docs/wiki/Home", nil, nil); code != 200 {
		t.Errorf("reader can't read: %d", code)
	}
	if code := asBob.api("PUT", "/api/v1/repos/admin/docs/wiki/Home", map[string]any{"content": "vandalism"}, nil); code != 403 {
		t.Errorf("reader edited: %d", code)
	}
	e.api("POST", "/api/v1/repos", map[string]any{"name": "secret", "private": true}, nil)
	e.api("PUT", "/api/v1/repos/admin/secret/wiki/Home", map[string]any{"content": "hidden"}, nil)
	if code := asBob.api("GET", "/api/v1/repos/admin/secret/wiki/Home", nil, nil); code != 404 {
		t.Errorf("stranger reads a private wiki: %d", code)
	}
	for _, bad := range []string{"..%2Fescape", ".hidden", "x%2F..%2Fy", "-flag"} {
		if code := e.api("PUT", "/api/v1/repos/admin/docs/wiki/"+bad, map[string]any{"content": "x"}, nil); code != 400 && code != 404 {
			t.Errorf("page name %q: %d", bad, code)
		}
	}

	// the wiki is a git repository: clone it, push a page, no CI runs
	work := t.TempDir()
	remote := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/docs.wiki.git"
	git(t, work, "clone", "-q", remote, "w")
	dir := filepath.Join(work, "w")
	var files []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && !strings.Contains(p, "/.git/") {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	if strings.Join(files, " ") != "Home.md Quick-Start.md guides/Setup.md" {
		t.Errorf("cloned wiki: %v", files)
	}
	os.WriteFile(filepath.Join(dir, "From-Git.md"), []byte("written locally"), 0o644)
	os.WriteFile(filepath.Join(dir, ".forge.yml"), []byte("name: x\non: {push: {}}\njobs:\n  a: {image: alpine, steps: [true]}\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.name=T", "-c", "user.email=t@x", "commit", "-q", "-m", "from git")
	git(t, dir, "push", "-q", "origin", "main")
	if got := names(); got != "Home, From Git, guides/Setup, Quick Start" {
		t.Errorf("after git push: %q", got)
	}
	var runs []any
	e.api("GET", "/api/v1/repos/admin/docs/runs", nil, &runs)
	if len(runs) != 0 {
		t.Errorf("a wiki push started CI: %v", runs)
	}
	// it is recorded as a wiki push, not as a push to the code
	var evs []struct{ Kind, Summary string }
	e.api("GET", "/api/v1/repos/admin/docs/events", nil, &evs)
	if len(evs) == 0 || evs[0].Kind != "wiki" || evs[0].Summary != "admin pushed to the wiki" {
		t.Errorf("latest event after a wiki push: %+v", evs)
	}
	for _, ev := range evs {
		if ev.Kind == "push" {
			t.Errorf("wiki push recorded as a code push: %+v", ev)
		}
	}
	// readers can clone, not push
	bobRemote := strings.Replace(e.srv.URL, "http://", "http://bob:"+bobTok+"@", 1) + "/admin/docs.wiki.git"
	git(t, work, "clone", "-q", bobRemote, "b")
	if out, err := exec.Command("git", "-C", filepath.Join(work, "b"), "push", "-q", "origin", "main:other").CombinedOutput(); err == nil {
		t.Errorf("reader pushed to the wiki: %s", out)
	}

	// over SSH
	f.Cfg.SSH.Listen = freePort(t)
	srv, err := sshd.New(f)
	if err != nil {
		t.Fatal(err)
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	signer, _ := newKey(t, t.TempDir(), "id")
	f.AddKey(ctx, e.admin, string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	var out string
	for i := 0; i < 50; i++ {
		if out, err = sshRun(f.Cfg.SSH.Listen, "git-upload-pack 'admin/docs.wiki.git'", signer); strings.Contains(out, "refs/heads/main") {
			break
		}
	}
	if !strings.Contains(out, "refs/heads/main") {
		t.Errorf("ssh upload-pack of the wiki: %v %q", err, out)
	}
	if out, _ := sshRun(f.Cfg.SSH.Listen, "git-upload-pack 'admin/secret2.wiki.git'", signer); !strings.Contains(out, "not found") {
		t.Errorf("missing repo's wiki: %q", out)
	}

	// delete, rename repo, delete repo
	if code := e.api("DELETE", "/api/v1/repos/admin/docs/wiki/From%20Git", nil, nil); code != 204 {
		t.Errorf("delete page: %d", code)
	}
	if code := e.api("PATCH", "/api/v1/repos/admin/docs", map[string]any{"name": "handbook"}, nil); code != 200 {
		t.Fatalf("rename repo: %d", code)
	}
	if got := func() string {
		var ps []page
		e.api("GET", "/api/v1/repos/admin/handbook/wiki", nil, &ps)
		return ps[0].Name
	}(); got != "Home" {
		t.Errorf("wiki after repo rename: %q", got)
	}
	r := mustRepo(t, f, "admin/handbook")
	wiki := f.WikiPath(r)
	if err := f.DeleteRepo(ctx, e.admin, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wiki); !os.IsNotExist(err) {
		t.Errorf("wiki kept after deleting the repo: %v", err)
	}
	if _, err := f.CreateRepo(ctx, e.admin, "admin", "clash.wiki", "", false); err == nil {
		t.Error("a repo named *.wiki would shadow a wiki")
	}
	if _, err := forge.WikiFile(" guides / My  Page "); err != nil {
		t.Error(err)
	}
	if fl, _ := forge.WikiFile(" guides / My  Page "); fl != "guides/My-Page.md" {
		t.Errorf("WikiFile: %q", fl)
	}
}

func mustRepo(t *testing.T, f *forge.Forge, full string) *store.Repo {
	t.Helper()
	owner, name, _ := strings.Cut(full, "/")
	r, err := f.Store.RepoByName(context.Background(), owner, name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

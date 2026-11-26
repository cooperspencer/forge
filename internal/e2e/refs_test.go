package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/protect"
)

// TestCreateRefs: branches and tags created from the forge (TUI, CLI, API).
func TestCreateRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	r, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	for _, m := range []string{"one", "two"} {
		os.WriteFile(filepath.Join(work, "f.txt"), []byte(m), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", m)
	}
	e.pushAs(t, work, "admin", e.token, "admin/app", "main")
	g := f.Git(r)
	sha := func(ref string) string { s, _ := g.ResolveRef(ctx, ref); return s }
	head, first := sha("refs/heads/main"), git(t, work, "rev-parse", "HEAD~1")

	// branches: from the default branch, a commit, a tag
	if err := f.CreateBranch(ctx, e.admin, r, "feature/x", ""); err != nil || sha("refs/heads/feature/x") != head {
		t.Errorf("branch from default: %v", err)
	}
	if err := f.CreateBranch(ctx, e.admin, r, "old", first[:10]); err != nil || sha("refs/heads/old") != first {
		t.Errorf("branch from a commit: %v", err)
	}
	// tags: lightweight and annotated
	if err := f.CreateTag(ctx, e.admin, r, "v0.1", "old", ""); err != nil || sha("refs/tags/v0.1") != first {
		t.Errorf("lightweight tag: %v", err)
	}
	if err := f.CreateTag(ctx, e.admin, r, "v1.0", "", "First stable"); err != nil || sha("refs/tags/v1.0") != head {
		t.Errorf("annotated tag: %v", err)
	}
	if typ := git(t, f.RepoPath(r), "cat-file", "-t", "refs/tags/v1.0"); typ != "tag" {
		t.Errorf("v1.0 is a %s, want an annotated tag", typ)
	}
	if typ := git(t, f.RepoPath(r), "cat-file", "-t", "refs/tags/v0.1"); typ != "commit" {
		t.Errorf("v0.1 is a %s, want a lightweight tag", typ)
	}
	if err := f.CreateBranch(ctx, e.admin, r, "from-tag", "v0.1"); err != nil || sha("refs/heads/from-tag") != first {
		t.Errorf("branch from a tag: %v", err)
	}
	var evs []struct{ Summary string }
	e.api("GET", "/api/v1/repos/admin/app/events", nil, &evs)
	if s := strings.Join(func() (o []string) {
		for _, ev := range evs {
			o = append(o, ev.Summary)
		}
		return
	}(), "\n"); !strings.Contains(s, "feature/x") || !strings.Contains(s, "v1.0") {
		t.Errorf("no push events for created refs:\n%s", s)
	}

	// refused
	for _, bad := range []string{"", "a b", "..x", "-x", "x.lock", "a..b", "x~1", "x:y", "HEAD", "a/HEAD", "x@{1}", "x/"} {
		if err := f.CreateBranch(ctx, e.admin, r, bad, ""); err == nil {
			t.Errorf("created branch %q", bad)
		}
		if err := f.CreateTag(ctx, e.admin, r, bad, "", ""); err == nil {
			t.Errorf("created tag %q", bad)
		}
	}
	if err := f.CreateBranch(ctx, e.admin, r, "old", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate branch: %v", err)
	}
	if err := f.CreateTag(ctx, e.admin, r, "v1.0", "", ""); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate tag: %v", err)
	}
	if err := f.CreateBranch(ctx, e.admin, r, "nowhere", "no-such-ref"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown source: %v", err)
	}
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	if err := f.CreateBranch(ctx, bob, r, "bobs", ""); err == nil {
		t.Error("a reader created a branch")
	}
	if err := f.CreateTag(ctx, bob, r, "bobs", "", ""); err == nil {
		t.Error("a reader created a tag")
	}
	// branch protection: only admins may create release/* branches
	f.Store.SetCollaborator(ctx, r.ID, bob.ID, "write")
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "release/*", Push: protect.PushAdmin})
	if err := f.CreateBranch(ctx, bob, r, "release/1", ""); err == nil || !strings.Contains(err.Error(), "only repository admins") {
		t.Errorf("writer creating a protected branch: %v", err)
	}
	if err := f.CreateBranch(ctx, bob, r, "bobs", ""); err != nil {
		t.Errorf("writer creating a branch: %v", err)
	}
	if err := f.CreateBranch(ctx, e.admin, r, "release/1", ""); err != nil {
		t.Errorf("admin creating a protected branch: %v", err)
	}
	// mirrors are read-only
	mr := *r
	mr.Mirror = true
	if err := f.CreateTag(ctx, e.admin, &mr, "v9", "", ""); err == nil {
		t.Error("tag in a mirror")
	}

	// API
	var out map[string]string
	if code := e.api("POST", "/api/v1/repos/admin/app/branches", map[string]string{"name": "api-branch", "from": "v0.1"}, &out); code != 201 || out["sha"] != first {
		t.Errorf("POST branches: %d %v", code, out)
	}
	if code := e.api("POST", "/api/v1/repos/admin/app/tags", map[string]string{"name": "api-tag", "message": "hi"}, &out); code != 201 || out["sha"] != head {
		t.Errorf("POST tags: %d %v", code, out)
	}
	if code := e.api("POST", "/api/v1/repos/admin/app/tags", map[string]string{"name": "api-tag"}, nil); code != 400 {
		t.Errorf("duplicate tag over the API: %d", code)
	}
	g2, _ := forge.GrantFrom("code:read", "")
	ro, _ := f.CreateToken(ctx, e.admin, forge.TokenSpec{Name: "ro", Grant: g2})
	asRO := &env{t: t, f: f, srv: e.srv, token: ro}
	if code := asRO.api("POST", "/api/v1/repos/admin/app/branches", map[string]string{"name": "ro"}, nil); code != 403 {
		t.Errorf("code:read token created a branch: %d", code)
	}
}

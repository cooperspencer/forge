package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

// TestRevert: commits and merged pull requests are undone with a new commit,
// directly on a branch or through a pull request.
func TestRevert(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	r, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	g := f.Git(r)
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	commit := func(file, content, msg string) string {
		t.Helper()
		os.WriteFile(filepath.Join(work, file), []byte(content), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", msg)
		return git(t, work, "rev-parse", "HEAD")
	}
	push := func(refspec string) { t.Helper(); e.pushAs(t, work, "admin", e.token, "admin/app", refspec) }
	file := func(rev, path string) string {
		t.Helper()
		b, _, err := g.Blob(ctx, rev, path, 1<<20)
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	head := func(b string) string { s, _ := g.ResolveRef(ctx, "refs/heads/"+b); return s }

	c1 := commit("a.txt", "a\n", "one")
	c2 := commit("b.txt", "b\n", "add b")
	c3 := commit("a.txt", "a2\n", "change a")
	push("main")

	// --- a commit, directly
	res, err := f.RevertCommit(ctx, e.admin, r, c2[:8], forge.RevertOptions{})
	if err != nil || res.Branch != "main" || res.PR != nil || head("main") != res.SHA {
		t.Fatalf("revert c2: %v %+v", err, res)
	}
	if file("main", "b.txt") != "<missing>" || file("main", "a.txt") != "a2\n" {
		t.Errorf("after reverting c2: b=%q a=%q", file("main", "b.txt"), file("main", "a.txt"))
	}
	rc, _ := g.Commit(ctx, res.SHA)
	if rc.Subject != `Revert "add b"` || !strings.Contains(rc.Body, "This reverts commit "+c2) || len(rc.Parents) != 1 || rc.Parents[0] != c3 || rc.Author != "admin" {
		t.Errorf("revert commit: %+v", rc)
	}
	if _, err := f.RevertCommit(ctx, e.admin, r, c2, forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "nothing to revert") {
		t.Errorf("reverting twice: %v", err)
	}
	if _, err := f.RevertCommit(ctx, e.admin, r, c1, forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "first commit") {
		t.Errorf("reverting the root: %v", err)
	}
	// later changes to the same lines conflict
	git(t, work, "pull", "-q", "--ff-only", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/app.git", "main")
	commit("a.txt", "a3\n", "change a again")
	push("main")
	before := head("main")
	if _, err := f.RevertCommit(ctx, e.admin, r, c3, forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "conflicts") || !strings.Contains(err.Error(), "a.txt") {
		t.Errorf("conflicting revert: %v", err)
	}
	if head("main") != before {
		t.Error("a failed revert moved main")
	}
	// only commits on the branch
	f.CreateBranch(ctx, e.admin, r, "old", c1)
	if _, err := f.RevertCommit(ctx, e.admin, r, c3, forge.RevertOptions{Branch: "old"}); err == nil || !strings.Contains(err.Error(), "not on old") {
		t.Errorf("revert of a commit not on the branch: %v", err)
	}
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	if _, err := f.RevertCommit(ctx, bob, r, before, forge.RevertOptions{}); err == nil || head("main") != before {
		t.Error("a reader reverted")
	}

	// --- merged pull requests
	openPR := func(branch, file, content string) int64 {
		t.Helper()
		git(t, work, "checkout", "-q", "-b", branch, "main")
		commit(file, content, "feature "+branch)
		push(branch)
		git(t, work, "checkout", "-q", "main")
		pr, err := f.OpenPR(ctx, e.admin, r, "main", r, branch, "Feature "+branch, "")
		if err != nil {
			t.Fatal(err)
		}
		return pr.Number
	}
	merge := func(n int64, method string) {
		t.Helper()
		pr, _ := f.Store.IssueByNumber(ctx, r.ID, n)
		if _, err := f.MergePR(ctx, e.admin, r, pr, method); err != nil {
			t.Fatal(err)
		}
	}
	reload := func(n int64) *store.Issue { i, _ := f.Store.IssueByNumber(ctx, r.ID, n); return i }

	m := openPR("feat-merge", "m.txt", "m\n")
	open := openPR("feat-open", "o.txt", "o\n")
	if _, err := f.RevertPR(ctx, e.admin, r, reload(open), forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Errorf("revert of an open PR: %v", err)
	}
	merge(m, "merge")
	if file("main", "m.txt") != "m\n" {
		t.Fatal("merge didn't land")
	}
	// main only takes pull requests now: a direct revert is refused…
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "main", Push: protect.PushNone})
	if _, err := f.RevertPR(ctx, e.admin, r, reload(m), forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "pull request instead") {
		t.Errorf("direct revert on a PR-only branch: %v", err)
	}
	// …but a revert pull request works, and merging it undoes the feature
	res, err = f.RevertPR(ctx, e.admin, r, reload(m), forge.RevertOptions{PullRequest: true})
	if err != nil || res.PR == nil || res.PR.PR.BaseBranch != "main" || !strings.HasPrefix(res.Branch, "revert-") || head(res.Branch) != res.SHA {
		t.Fatalf("revert PR: %v %+v", err, res)
	}
	if !strings.HasPrefix(res.PR.Title, `Revert "Feature feat-merge" (#`) || file(res.Branch, "m.txt") != "<missing>" || file("main", "m.txt") != "m\n" {
		t.Errorf("revert PR content: %q, m.txt on branch %q, on main %q", res.PR.Title, file(res.Branch, "m.txt"), file("main", "m.txt"))
	}
	merge(res.PR.Number, "merge")
	if file("main", "m.txt") != "<missing>" {
		t.Error("merging the revert PR didn't undo the feature")
	}
	// a second revert PR of the same commit gets its own branch name
	f.RemoveProtection(ctx, e.admin, r, "main")
	taken := "revert-" + res.SHA[:8]
	f.CreateBranch(ctx, e.admin, r, taken, "main")
	if res, err := f.RevertCommit(ctx, e.admin, r, res.SHA, forge.RevertOptions{PullRequest: true}); err != nil || res.PR == nil || res.Branch != taken+"-2" {
		t.Errorf("revert of a revert as a PR: %v %+v", err, res)
	}

	sq := openPR("feat-squash", "s.txt", "s\n")
	merge(sq, "squash")
	if res, err := f.RevertPR(ctx, e.admin, r, reload(sq), forge.RevertOptions{}); err != nil || file("main", "s.txt") != "<missing>" || res.Branch != "main" {
		t.Errorf("revert of a squashed PR: %v", err)
	}
	git(t, work, "fetch", "-q", strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1)+"/admin/app.git", "main")
	git(t, work, "reset", "-q", "--hard", "FETCH_HEAD")
	ff := openPR("feat-ff", "f.txt", "f\n")
	merge(ff, "ff")
	if _, err := f.RevertPR(ctx, e.admin, r, reload(ff), forge.RevertOptions{}); err == nil || !strings.Contains(err.Error(), "fast-forwarded") {
		t.Errorf("revert of a fast-forwarded PR: %v", err)
	}

	// --- API
	var out map[string]any
	ffHead := head("main")
	if code := e.api("POST", "/api/v1/repos/admin/app/commits/"+ffHead+"/revert", map[string]any{}, &out); code != 201 || out["branch"] != "main" || file("main", "f.txt") != "<missing>" {
		t.Errorf("POST commit revert: %d %v", code, out)
	}
	merge(open, "merge")
	n := open
	if code := e.api("POST", "/api/v1/repos/admin/app/pulls/"+strconv.FormatInt(n, 10)+"/revert", map[string]any{"pull_request": true}, &out); code != 201 || out["pull_request"] == nil {
		t.Errorf("POST PR revert: %d %v", code, out)
	}
	cw, _ := forge.GrantFrom("code:write issues:read", "")
	tok, _ := f.CreateToken(ctx, e.admin, forge.TokenSpec{Name: "cw", Grant: cw})
	asCW := &env{t: t, f: f, srv: e.srv, token: tok}
	if code := asCW.api("POST", "/api/v1/repos/admin/app/commits/"+c3+"/revert", map[string]any{"pull_request": true}, nil); code != 403 {
		t.Errorf("revert PR without issues:write: %d", code)
	}
}

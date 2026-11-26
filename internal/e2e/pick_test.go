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

// TestCherryPick: commits and merged pull requests are applied to other
// branches, keeping the original author.
func TestCherryPick(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	r, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	g := f.Git(r)
	work := t.TempDir()
	remote := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/app.git"
	git(t, work, "init", "-q", "-b", "main")
	commit := func(file, content, msg string) string {
		t.Helper()
		os.WriteFile(filepath.Join(work, file), []byte(content), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", msg)
		return git(t, work, "rev-parse", "HEAD")
	}
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
	git(t, work, "branch", "release")
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "--date=2020-01-02T03:04:05Z", "-m", "add b\n\nwith a body")
	c2 := git(t, work, "rev-parse", "HEAD")
	c3 := commit("a.txt", "a2\n", "change a")
	git(t, work, "push", "-q", remote, "main", "release")

	// --- a commit, directly
	res, err := f.CherryPick(ctx, e.admin, r, c2, forge.PickOptions{Branch: "release"})
	if err != nil || res.Branch != "release" || head("release") != res.SHA {
		t.Fatalf("pick c2: %v %+v", err, res)
	}
	if file("release", "b.txt") != "b\n" || file("release", "a.txt") != "a\n" {
		t.Errorf("after picking c2: b=%q a=%q", file("release", "b.txt"), file("release", "a.txt"))
	}
	orig, _ := g.Commit(ctx, c2)
	pc, _ := g.Commit(ctx, res.SHA)
	if pc.Subject != "add b" || !strings.Contains(pc.Body, "with a body") || !strings.Contains(pc.Body, "(cherry picked from commit "+c2+")") ||
		pc.Author != "T" || pc.AuthorEmail != "t@x" || !pc.When.Equal(orig.When) || orig.When.Year() != 2020 || len(pc.Parents) != 1 || pc.Parents[0] != c1 {
		t.Errorf("picked commit: %+v", pc)
	}
	if committer := git(t, f.RepoPath(r), "log", "-1", "--format=%cn", res.SHA); committer != "admin" {
		t.Errorf("committer %q", committer)
	}
	if _, err := f.CherryPick(ctx, e.admin, r, c2, forge.PickOptions{Branch: "release"}); err == nil || !strings.Contains(err.Error(), "already on release") {
		t.Errorf("picking twice: %v", err)
	}
	if _, err := f.CherryPick(ctx, e.admin, r, c1, forge.PickOptions{Branch: "main"}); err == nil || !strings.Contains(err.Error(), "already on main") {
		t.Errorf("picking a commit the branch has: %v", err)
	}
	if _, err := f.CherryPick(ctx, e.admin, r, c3, forge.PickOptions{}); err == nil || !strings.Contains(err.Error(), "which branch") {
		t.Errorf("pick without a branch: %v", err)
	}
	// conflicting changes on the target
	git(t, work, "fetch", "-q", remote, "release")
	git(t, work, "checkout", "-q", "-b", "rel", "FETCH_HEAD")
	commit("a.txt", "r\n", "release-only change")
	git(t, work, "push", "-q", remote, "rel:release")
	git(t, work, "checkout", "-q", "main")
	before := head("release")
	if _, err := f.CherryPick(ctx, e.admin, r, c3, forge.PickOptions{Branch: "release"}); err == nil || !strings.Contains(err.Error(), "conflicts") || !strings.Contains(err.Error(), "a.txt") {
		t.Errorf("conflicting pick: %v", err)
	}
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	f.CreateBranch(ctx, e.admin, r, "old", c1)
	if _, err := f.CherryPick(ctx, bob, r, c2, forge.PickOptions{Branch: "old"}); err == nil || head("old") != c1 {
		t.Error("a reader cherry-picked")
	}
	if head("release") != before {
		t.Error("a failed pick moved release")
	}

	// --- merged pull requests (backports)
	openPR := func(branch, file, content string) int64 {
		t.Helper()
		git(t, work, "fetch", "-q", remote, "main")
		git(t, work, "checkout", "-q", "-B", branch, "FETCH_HEAD")
		commit(file, content, "feature "+branch)
		git(t, work, "push", "-q", remote, branch)
		pr, err := f.OpenPR(ctx, e.admin, r, "main", r, branch, "Feature "+branch, "Body of "+branch)
		if err != nil {
			t.Fatal(err)
		}
		return pr.Number
	}
	issue := func(n int64) *store.Issue { i, _ := f.Store.IssueByNumber(ctx, r.ID, n); return i }
	merge := func(n int64, method string) {
		t.Helper()
		if _, err := f.MergePR(ctx, e.admin, r, issue(n), method); err != nil {
			t.Fatal(err)
		}
	}
	m := openPR("feat-merge", "m.txt", "m\n")
	if _, err := f.CherryPickPR(ctx, e.admin, r, issue(m), forge.PickOptions{Branch: "release"}); err == nil || !strings.Contains(err.Error(), "merged") {
		t.Errorf("pick of an open PR: %v", err)
	}
	merge(m, "merge")
	if _, err := f.CherryPickPR(ctx, e.admin, r, issue(m), forge.PickOptions{Branch: "main"}); err == nil || !strings.Contains(err.Error(), "other than main") {
		t.Errorf("pick of a PR onto its own base: %v", err)
	}
	// release only takes pull requests: direct picks are refused, PRs work
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "release", Push: protect.PushNone})
	if _, err := f.CherryPickPR(ctx, e.admin, r, issue(m), forge.PickOptions{Branch: "release"}); err == nil || !strings.Contains(err.Error(), "pull request instead") {
		t.Errorf("direct pick onto a PR-only branch: %v", err)
	}
	res, err = f.CherryPickPR(ctx, e.admin, r, issue(m), forge.PickOptions{Branch: "release", PullRequest: true})
	if err != nil || res.PR == nil || res.PR.PR.BaseBranch != "release" || !strings.HasPrefix(res.Branch, "cherry-pick-") {
		t.Fatalf("backport PR: %v %+v", err, res)
	}
	if res.PR.Title != "Feature feat-merge (#"+strconv.FormatInt(m, 10)+")" || !strings.Contains(res.PR.Body, "cherry picked from pull request #") ||
		file(res.Branch, "m.txt") != "m\n" || file(res.Branch, "a.txt") != "r\n" || file("release", "m.txt") != "<missing>" {
		t.Errorf("backport PR content: %q %q m=%q a=%q", res.PR.Title, res.PR.Body, file(res.Branch, "m.txt"), file(res.Branch, "a.txt"))
	}
	f.RemoveProtection(ctx, e.admin, r, "release")

	sq := openPR("feat-squash", "s.txt", "s\n")
	merge(sq, "squash")
	if res, err := f.CherryPickPR(ctx, e.admin, r, issue(sq), forge.PickOptions{Branch: "release"}); err != nil || file("release", "s.txt") != "s\n" || res.PR != nil {
		t.Errorf("pick of a squashed PR: %v", err)
	}
	ff := openPR("feat-ff", "f.txt", "f\n")
	merge(ff, "ff")
	if _, err := f.CherryPickPR(ctx, e.admin, r, issue(ff), forge.PickOptions{Branch: "release"}); err == nil || !strings.Contains(err.Error(), "fast-forwarded") {
		t.Errorf("pick of a fast-forwarded PR: %v", err)
	}

	// --- API
	var out map[string]any
	if code := e.api("POST", "/api/v1/repos/admin/app/commits/"+head("main")+"/cherry-pick", map[string]any{"branch": "release"}, &out); code != 201 || out["branch"] != "release" || file("release", "f.txt") != "f\n" {
		t.Errorf("POST cherry-pick: %d %v", code, out)
	}
	if code := e.api("POST", "/api/v1/repos/admin/app/commits/"+c3+"/cherry-pick", map[string]any{}, nil); code != 400 {
		t.Errorf("cherry-pick without a branch: %d", code)
	}
	e2 := openPR("feat-api", "x.txt", "x\n")
	merge(e2, "merge")
	if code := e.api("POST", "/api/v1/repos/admin/app/pulls/"+strconv.FormatInt(e2, 10)+"/cherry-pick", map[string]any{"branch": "release", "pull_request": true}, &out); code != 201 || out["pull_request"] == nil {
		t.Errorf("POST PR cherry-pick: %d %v", code, out)
	}
}

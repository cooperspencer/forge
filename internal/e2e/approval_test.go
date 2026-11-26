package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

// TestForkApproval: pull requests from forks by people without write access
// wait for approval before anything runs; a newer push supersedes the held
// run; only people with write access can approve; collaborators' fork PRs
// run right away.
func TestForkApproval(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	e.startRunner(t, "shell")

	base, err := f.CreateRepo(ctx, e.admin, "admin", "proj", "", false)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, ".forge.yml"), []byte("name: ci\njobs:\n  check:\n    image: alpine:3\n    steps: [\"echo ran $FORGE_SHA\"]\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "init")
	e.pushAs(t, work, "admin", e.token, "admin/proj", "main")
	e.waitRun("admin/proj", 1, store.StatusSuccess)

	// An outsider forks and opens a PR.
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	fork, err := f.Fork(ctx, bob, base, "bob")
	if err != nil {
		t.Fatal(err)
	}
	git(t, work, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(work, "x"), []byte("1"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "feature")
	e.pushAs(t, work, "bob", bobTok, "bob/proj", "feature")
	pr, err := f.OpenPR(ctx, bob, base, "main", fork, "feature", "", "")
	if err != nil {
		t.Fatal(err)
	}
	held := e.waitRun("admin/proj", 2, store.StatusApproval)
	time.Sleep(1500 * time.Millisecond) // give the runner a chance to (wrongly) pick it up
	run, _ := f.Store.RunByNumber(ctx, base.ID, 2)
	if run.Status != store.StatusApproval || run.PRNumber != pr.Number {
		t.Fatalf("held run: %+v", run)
	}
	for _, j := range held.Jobs {
		if j.Status != store.StatusApproval {
			t.Fatalf("job of held run is %s", j.Status)
		}
	}

	// bob can't approve his own run.
	if err := f.ApproveRun(ctx, bob, base, run); err == nil {
		t.Fatal("PR author without write access approved the run")
	}
	bobAPI := &env{t: t, f: f, srv: e.srv, token: bobTok}
	if c := bobAPI.api("POST", "/api/v1/repos/admin/proj/runs/2/approve", nil, nil); c != 403 {
		t.Errorf("bob approve via API: %d", c)
	}

	// A new push supersedes the held run.
	os.WriteFile(filepath.Join(work, "x"), []byte("2"), 0o644)
	git(t, work, "commit", "-q", "-am", "more")
	e.pushAs(t, work, "bob", bobTok, "bob/proj", "feature")
	e.waitRun("admin/proj", 3, store.StatusApproval)
	e.waitRun("admin/proj", 2, store.StatusCancelled)

	// The maintainer approves: it runs, still without secrets.
	if c := e.api("POST", "/api/v1/repos/admin/proj/runs/3/approve", nil, nil); c != 202 {
		t.Fatalf("approve: %d", c)
	}
	done := e.waitRun("admin/proj", 3, store.StatusSuccess)
	j, _ := f.Store.JobByID(ctx, done.Jobs[0].ID)
	if !strings.Contains(j.Spec, `"fork":true`) {
		t.Error("approved fork run lost its fork flag (would get secrets / push rights)")
	}
	if err := f.ApproveRun(ctx, e.admin, base, run); err == nil {
		t.Error("approved a run that is not waiting")
	}

	// A collaborator with write access is trusted: no hold.
	carol, _ := f.Store.CreateUser(ctx, "carol", false, false)
	carolTok, _ := f.Store.CreateAccessToken(ctx, carol.ID, "t")
	f.Store.SetCollaborator(ctx, base.ID, carol.ID, "write")
	cfork, err := f.Fork(ctx, carol, base, "carol")
	if err != nil {
		t.Fatal(err)
	}
	git(t, work, "checkout", "-q", "-b", "carol-change", "main")
	os.WriteFile(filepath.Join(work, "y"), []byte("1"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "carol")
	e.pushAs(t, work, "carol", carolTok, "carol/proj", "carol-change")
	if _, err := f.OpenPR(ctx, carol, base, "main", cfork, "carol-change", "", ""); err != nil {
		t.Fatal(err)
	}
	e.waitRun("admin/proj", 4, store.StatusSuccess)
}

// pushAs pushes branch over smart HTTP as user (token as password).
func (e *env) pushAs(t *testing.T, dir, user, token, repo, branch string) {
	t.Helper()
	remote := strings.Replace(e.srv.URL, "http://", "http://"+user+":"+token+"@", 1) + "/" + repo + ".git"
	git(t, dir, "push", "-q", "-f", remote, branch)
}

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInterdiffGit: without jj change IDs, commits are matched across force
// pushes by identical patches; changed commits show up as dropped + new.
func TestInterdiffGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	r, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	work := t.TempDir()
	remote := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/app.git"
	commit := func(file, content, msg string) {
		t.Helper()
		os.WriteFile(filepath.Join(work, file), []byte(content), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", msg)
	}
	git(t, work, "init", "-q", "-b", "main")
	commit("base.txt", "base\n", "base")
	git(t, work, "push", "-q", remote, "main")
	git(t, work, "checkout", "-q", "-b", "feat")
	commit("a.txt", "a\n", "one")
	commit("b.txt", "b\n", "two")
	git(t, work, "push", "-q", remote, "feat")
	pr, err := f.OpenPR(ctx, e.admin, r, "main", r, "feat", "", "")
	if err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		i, _ := f.Store.IssueByNumber(ctx, r.ID, pr.Number)
		d, err := f.PRInterdiff(ctx, e.admin, r, i, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range d.Changes {
			subj := ""
			if c.New != nil {
				subj = c.New.Subject
			} else {
				subj = c.Old.Subject
			}
			out = append(out, subj+"="+c.Status)
		}
		return strings.Join(out, ", ")
	}
	if i, _ := f.Store.IssueByNumber(ctx, r.ID, pr.Number); i != nil {
		if _, err := f.PRInterdiff(ctx, e.admin, r, i, 0, 0); err == nil || !strings.Contains(err.Error(), "only one revision") {
			t.Errorf("interdiff of one revision: %v", err)
		}
	}
	// amend the last commit: no change IDs, so it can't be matched
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b2\n"), 0o644)
	git(t, work, "commit", "-q", "-a", "--amend", "-m", "two")
	git(t, work, "push", "-q", "-f", remote, "feat")
	if s := status(); s != "one=unchanged, two=new, two=dropped" {
		t.Errorf("after amending: %s", s)
	}
	// main moves, rebase: identical patches are matched
	git(t, work, "checkout", "-q", "main")
	commit("m.txt", "m\n", "main moves")
	git(t, work, "push", "-q", remote, "main")
	git(t, work, "checkout", "-q", "feat")
	git(t, work, "rebase", "-q", "main")
	git(t, work, "push", "-q", "-f", remote, "feat")
	if s := status(); s != "one=rebased, two=rebased" {
		t.Errorf("after rebasing: %s", s)
	}
	// a pushed commit on top
	commit("c.txt", "c\n", "three")
	git(t, work, "push", "-q", remote, "feat")
	if s := status(); s != "one=unchanged, two=unchanged, three=new" {
		t.Errorf("after a new commit: %s", s)
	}
}

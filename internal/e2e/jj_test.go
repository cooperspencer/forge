package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
)

// TestJujutsu drives the real jj CLI (set FORGE_TEST_JJ=/path/to/jj, or have
// jj in PATH): change IDs, pull request revisions and interdiffs across jj
// rewrites (amend, reword, rebase, abandon), and stacked pull requests.
func TestJujutsu(t *testing.T) {
	bin := os.Getenv("FORGE_TEST_JJ")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("jj"); err != nil {
			t.Skip("jj not installed (set FORGE_TEST_JJ)")
		}
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	r, _ := f.CreateRepo(ctx, e.admin, "admin", "jj", "", false)
	home := t.TempDir()
	cfg := filepath.Join(home, "jj.toml")
	os.WriteFile(cfg, []byte("[user]\nname = \"Jay\"\nemail = \"jay@example.com\"\n[ui]\npaginate = \"never\"\ncolor = \"never\"\n"), 0o644)
	dir := filepath.Join(home, "repo")
	jj := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Dir = dir
		if args[0] == "git" && args[1] == "clone" {
			c.Dir = home
		}
		c.Env = append(os.Environ(), "HOME="+home, "JJ_CONFIG="+cfg, "GIT_TERMINAL_PROMPT=0")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("jj %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) { os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644) }
	changeID := func(rev string) string { return jj("log", "-r", rev, "--no-graph", "-T", "change_id") }
	url := strings.Replace(e.srv.URL, "http://", "http://admin:"+e.token+"@", 1) + "/admin/jj.git"

	jj("git", "clone", url, "repo")
	write("base.txt", "base\n")
	jj("commit", "-m", "base")
	jj("bookmark", "create", "main", "-r", "@-")
	jj("git", "push", "--bookmark", "main")
	// two changes A and B on a bookmark
	write("a.txt", "a\n")
	jj("commit", "-m", "change A")
	write("b.txt", "b\n")
	jj("commit", "-m", "change B")
	idA, idB := changeID("@--"), changeID("@-")
	jj("bookmark", "create", "feat", "-r", "@-")
	jj("git", "push", "--bookmark", "feat")

	pr, err := f.OpenPR(ctx, e.admin, r, "main", r, "feat", "", "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.PRStatus(ctx, r, pr)
	if len(st.Commits) != 2 || st.Commits[0].ChangeID != idB || st.Commits[1].ChangeID != idA {
		t.Fatalf("change IDs of PR commits: %+v (want %s, %s)", st.Commits, idB, idA)
	}
	interdiff := func(from, to int) *forge.Interdiff {
		t.Helper()
		i, _ := f.Store.IssueByNumber(ctx, r.ID, pr.Number)
		d, err := f.PRInterdiff(ctx, e.admin, r, i, from, to)
		if err != nil {
			t.Fatalf("interdiff %d..%d: %v", from, to, err)
		}
		return d
	}
	statuses := func(d *forge.Interdiff) string {
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
	files := func(fs []gitx.FileDiff) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.NewPath)
		}
		return strings.Join(out, ",")
	}

	// rev 2: amend A (jj rebases B onto the new A)
	jj("edit", idA)
	write("a.txt", "a, improved\n")
	jj("new", idB)
	jj("git", "push", "--bookmark", "feat")
	d := interdiff(1, 2)
	if s := statuses(d); s != "change A=rewritten, change B=rebased" {
		t.Errorf("after amending A: %s", s)
	}
	if d.Rebased || files(d.Files) != "a.txt" || len(d.Changes[0].Files) != 1 || !strings.Contains(d.Changes[0].Files[0].Patch, "+a, improved") {
		t.Errorf("amend interdiff: rebased=%v files=%s change files=%+v", d.Rebased, files(d.Files), d.Changes[0].Files)
	}
	if d.Changes[0].ChangeID != idA || d.Changes[0].Old.SHA == d.Changes[0].New.SHA {
		t.Errorf("change A across revisions: %+v", d.Changes[0])
	}

	// rev 3: reword B
	jj("describe", "-r", idB, "-m", "change B, reworded")
	jj("git", "push", "--bookmark", "feat")
	if s := statuses(interdiff(2, 3)); s != "change A=unchanged, change B, reworded=reworded" {
		t.Errorf("after rewording B: %s", s)
	}

	// rev 4: main moves on, the stack is rebased onto it
	jj("new", "main")
	write("m.txt", "m\n")
	jj("commit", "-m", "main moves")
	jj("bookmark", "set", "main", "-r", "@-")
	jj("git", "push", "--bookmark", "main")
	jj("rebase", "-s", idA, "-d", "main")
	jj("git", "push", "--bookmark", "feat")
	d = interdiff(3, 4)
	if s := statuses(d); !d.Rebased || s != "change A=rebased, change B, reworded=rebased" || len(d.Files) != 0 {
		t.Errorf("after rebasing onto main: rebased=%v %s files=%s", d.Rebased, s, files(d.Files))
	}
	// …and the default is the last push
	if def := interdiff(0, 0); def.From.N != 3 || def.To.N != 4 {
		t.Errorf("default interdiff: %d..%d", def.From.N, def.To.N)
	}

	// rev 5: main moves again; A is amended and rebased in one go. The
	// change's own interdiff shows only the amendment, not main's commit.
	jj("new", "main")
	write("n.txt", "n\n")
	jj("commit", "-m", "main moves again")
	jj("bookmark", "set", "main", "-r", "@-")
	jj("git", "push", "--bookmark", "main")
	jj("rebase", "-s", idA, "-d", "main")
	jj("edit", idA)
	write("a.txt", "a, improved twice\n")
	jj("new", idB)
	jj("git", "push", "--bookmark", "feat")
	d = interdiff(4, 5)
	if s := statuses(d); !d.Rebased || s != "change A=rewritten, change B, reworded=rebased" {
		t.Errorf("after amending and rebasing: rebased=%v %s", d.Rebased, s)
	}
	if got := files(d.Changes[0].Files); got != "a.txt" || files(d.Files) != "a.txt" {
		t.Errorf("amend+rebase interdiff: change %s, whole %s", got, files(d.Files))
	}

	// rev 6: abandon B (feat moves to A)
	jj("abandon", "--retain-bookmarks", idB)
	jj("git", "push", "--bookmark", "feat")
	if s := statuses(interdiff(5, 6)); s != "change A=unchanged, change B, reworded=dropped" {
		t.Errorf("after abandoning B: %s", s)
	}
	revs, _ := f.Store.PRRevisions(ctx, pr.ID)
	if len(revs) != 6 {
		t.Errorf("revisions: %+v", revs)
	}
	if n, _ := f.Store.AddPRRevision(ctx, pr.ID, revs[5].HeadSHA, revs[5].BaseSHA, e.admin.ID); n != 0 {
		t.Errorf("the same head recorded twice as revision %d", n)
	}
	// old revisions survive garbage collection
	git(t, f.RepoPath(r), "gc", "-q", "--prune=now")
	if _, err := f.Git(r).Commit(ctx, revs[0].HeadSHA); err != nil {
		t.Errorf("revision 1 is gone after gc: %v", err)
	}

	// --- stacked pull requests: C on top of feat
	jj("new", idA)
	write("c.txt", "c\n")
	jj("commit", "-m", "change C")
	jj("bookmark", "create", "feat2", "-r", "@-")
	jj("git", "push", "--bookmark", "feat2")
	pr2, err := f.OpenPR(ctx, e.admin, r, "feat", r, "feat2", "", "")
	if err != nil {
		t.Fatal(err)
	}
	pr, _ = f.Store.IssueByNumber(ctx, r.ID, pr.Number)
	below, above := f.PRStack(ctx, r, pr2)
	if len(below) != 1 || below[0].Number != pr.Number || len(above) != 0 {
		t.Errorf("stack of #%d: below %v above %v", pr2.Number, below, above)
	}
	if b, a := f.PRStack(ctx, r, pr); len(b) != 0 || len(a) != 1 || a[0].Number != pr2.Number {
		t.Errorf("stack of #%d: below %v above %v", pr.Number, b, a)
	}
	if _, err := f.MergePR(ctx, e.admin, r, pr, "merge"); err != nil {
		t.Fatal(err)
	}
	pr2, _ = f.Store.IssueByNumber(ctx, r.ID, pr2.Number)
	if pr2.PR.BaseBranch != "main" {
		t.Errorf("stacked PR not retargeted: base %q", pr2.PR.BaseBranch)
	}
	if st, _ := f.PRStatus(ctx, r, pr2); len(st.Commits) != 1 || st.Commits[0].Subject != "change C" {
		t.Errorf("retargeted PR commits: %+v", st.Commits)
	}

	// --- API
	var apiRevs []map[string]any
	if e.api("GET", fmt.Sprintf("/api/v1/repos/admin/jj/pulls/%d/revisions", pr.Number), nil, &apiRevs); len(apiRevs) != 6 {
		t.Errorf("GET revisions: %v", apiRevs)
	}
	var id map[string]any
	if code := e.api("GET", fmt.Sprintf("/api/v1/repos/admin/jj/pulls/%d/interdiff?from=1&to=2", pr.Number), nil, &id); code != 200 {
		t.Fatalf("GET interdiff: %d", code)
	}
	ch := id["changes"].([]any)[0].(map[string]any)
	if ch["status"] != "rewritten" || ch["change_id"] != idA || ch["files"] == nil {
		t.Errorf("interdiff change: %v", ch)
	}
	var commits []map[string]any
	e.api("GET", "/api/v1/repos/admin/jj/commits?ref=main&limit=3", nil, &commits)
	if len(commits) != 3 || commits[0]["change_id"] != nil || commits[1]["change_id"] == nil {
		t.Errorf("change IDs in the commit list (the merge commit forge made has none): %v", commits)
	}
}

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

	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/sshd"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// TestBranchProtection: pushes over HTTP and SSH go through the pre-receive
// hook; merges and branch deletion check the same rules.
func TestBranchProtection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f
	stopRunner := e.startRunner(t, "shell")
	defer stopRunner()

	r, err := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	f.Store.SetCollaborator(ctx, r.ID, bob.ID, "write")

	work := t.TempDir()
	run := func(dir string, env []string, args ...string) (string, error) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x", "GIT_TERMINAL_PROMPT=0"), env...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	commit := func(msg string) {
		t.Helper()
		os.WriteFile(filepath.Join(work, "f.txt"), []byte(msg), 0o644)
		git(t, work, "add", ".")
		git(t, work, "commit", "-q", "-m", msg)
	}
	remote := func(user, tok string) string {
		return strings.Replace(e.srv.URL, "http://", "http://"+user+":"+tok+"@", 1) + "/admin/app.git"
	}
	admin, writer := remote("admin", e.token), remote("bob", bobTok)
	push := func(url string, refspec ...string) (string, error) {
		return run(work, nil, append([]string{"push", url}, refspec...)...)
	}

	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, ".forge.yml"), []byte("name: ci\non: {push: {}}\njobs:\n  t: {image: alpine, steps: [\"true\"]}\n"), 0o644)
	commit("one")
	if out, err := push(admin, "main", "main:release/1"); err != nil {
		t.Fatalf("initial push: %v\n%s", err, out)
	}

	// --- pull requests only
	if err := f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "main", Push: protect.PushNone, Approvals: 1, Checks: []string{"ci"}, BlockOnChanges: true}); err != nil {
		t.Fatal(err)
	}
	commit("two")
	out, err := push(admin, "main")
	if err == nil || !strings.Contains(out, "main is protected: changes must go through a pull request") || !strings.Contains(out, "pr create admin/app") {
		t.Errorf("direct push to a PR-only branch: %v\n%s", err, out)
	}
	if out, err := push(admin, "HEAD:feature"); err != nil {
		t.Fatalf("push to an unprotected branch: %v\n%s", err, out)
	}
	if out, err := push(admin, "--tags"); err != nil {
		t.Errorf("tags are not branches: %v\n%s", err, out)
	}

	// --- merging needs an approval (not the author's) and the ci check
	pr, err := f.OpenPR(ctx, e.admin, r, "main", r, "feature", "two", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.MergePR(ctx, e.admin, r, pr, "merge")
	if err == nil || !strings.Contains(err.Error(), "needs 1 approval(s), has 0") {
		t.Errorf("merge without approval: %v", err)
	}
	f.Comment(ctx, e.admin, r, pr, &store.Comment{Review: "approve", Body: "self"})
	if _, err := f.MergePR(ctx, e.admin, r, pr, "merge"); err == nil || !strings.Contains(err.Error(), "has 0") {
		t.Errorf("self-approval counted: %v", err)
	}
	f.Comment(ctx, bob, r, pr, &store.Comment{Review: "changes", Body: "hmm"})
	if _, err := f.MergePR(ctx, e.admin, r, pr, "merge"); err == nil || !strings.Contains(err.Error(), "changes requested by bob") {
		t.Errorf("merge with requested changes: %v", err)
	}
	f.Comment(ctx, bob, r, pr, &store.Comment{Review: "approve", Body: "ok"})
	// the feature push ran ci; wait for that run by its ref, not its number
	// (the first push started one run per branch, so the numbers depend on
	// how many branches it had)
	var featureRun int64
	runs, _ := f.Store.Runs(ctx, r.ID, 10)
	for _, x := range runs {
		if x.Ref == "refs/heads/feature" {
			featureRun = x.Number
		}
	}
	if featureRun == 0 {
		t.Fatal("the push to feature started no pipeline")
	}
	e.waitRun("admin/app", featureRun, store.StatusSuccess)
	pr, _ = f.Store.IssueByNumber(ctx, r.ID, pr.Number)
	if _, err := f.MergePR(ctx, e.admin, r, pr, "merge"); err != nil {
		t.Errorf("merge with approval and checks: %v", err)
	}

	// --- direct pushes allowed, history may not be rewritten
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "main", Push: protect.PushWrite})
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "release/*", Push: protect.PushAdmin})
	git(t, work, "fetch", "-q", admin, "main")
	git(t, work, "reset", "-q", "--hard", "FETCH_HEAD")
	commit("three")
	if out, err := push(writer, "main"); err != nil {
		t.Errorf("fast-forward push: %v\n%s", err, out)
	}
	git(t, work, "reset", "-q", "--hard", "HEAD~1")
	commit("rewritten")
	out, err = push(admin, "+main")
	if err == nil || !strings.Contains(out, "force pushes") {
		t.Errorf("force push: %v\n%s", err, out)
	}
	out, err = push(writer, ":release/1")
	if err == nil || !strings.Contains(out, "release/1 is protected and can't be deleted") {
		t.Errorf("delete a protected branch: %v\n%s", err, out)
	}
	out, err = push(writer, "HEAD:release/2")
	if err == nil || !strings.Contains(out, "only repository admins") {
		t.Errorf("writer creating a release branch: %v\n%s", err, out)
	}
	if out, err := push(admin, "HEAD:release/2"); err != nil {
		t.Errorf("admin creating a release branch: %v\n%s", err, out)
	}
	// one bad ref rejects the whole push
	out, err = push(writer, "HEAD:topic", ":release/1")
	if err == nil {
		t.Errorf("mixed push accepted:\n%s", out)
	}
	if _, err := f.Git(r).ResolveRef(ctx, "refs/heads/topic"); err == nil {
		t.Error("the good half of a rejected push landed")
	}
	if err := f.DeleteBranch(ctx, e.admin, r, "release/1"); err == nil || !strings.Contains(err.Error(), "can't be deleted") {
		t.Errorf("TUI delete of a protected branch: %v", err)
	}
	if err := f.DeleteBranch(ctx, e.admin, r, "feature"); err != nil {
		t.Errorf("delete an unprotected branch: %v", err)
	}

	// --- admins bypass, if the rule says so
	f.SetProtection(ctx, e.admin, r, protect.Rule{Pattern: "main", Push: protect.PushNone, AdminBypass: true})
	if out, err := push(admin, "+main"); err != nil {
		t.Errorf("admin bypass: %v\n%s", err, out)
	}
	commit("four")
	if out, err := push(writer, "main"); err == nil {
		t.Errorf("writer is not an admin:\n%s", out)
	}

	// --- over SSH: the same hook
	if _, err := exec.LookPath("ssh"); err == nil {
		f.Cfg.SSH.Listen = freePort(t)
		srv, err := sshd.New(f)
		if err != nil {
			t.Fatal(err)
		}
		go srv.ListenAndServe()
		t.Cleanup(func() { srv.Shutdown(context.Background()) })
		signer, keyPath := newKey(t, t.TempDir(), "id_bob")
		f.AddKey(ctx, bob, string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
		_, port, _ := net.SplitHostPort(f.Cfg.SSH.Listen)
		sshEnv := []string{fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -p %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o LogLevel=ERROR", keyPath, port)}
		out, err := run(work, sshEnv, "push", "ssh://git@127.0.0.1:"+port+"/admin/app.git", "HEAD:main")
		if err == nil || !strings.Contains(out, "main is protected: changes must go through a pull request") {
			t.Errorf("ssh push to a protected branch: %v\n%s", err, out)
		}
		if out, err := run(work, sshEnv, "push", "ssh://git@127.0.0.1:"+port+"/admin/app.git", "HEAD:ssh-topic"); err != nil {
			t.Errorf("ssh push to an unprotected branch: %v\n%s", err, out)
		}
	}

	// --- removing the rules makes the repository hook-free again
	f.RemoveProtection(ctx, e.admin, r, "main")
	f.RemoveProtection(ctx, e.admin, r, "release/*")
	if cfg, env := f.PushHook(ctx, e.admin, r); cfg != nil || env != nil {
		t.Errorf("hook without rules: %v %v", cfg, env)
	}
	if out, err := push(writer, ":release/1"); err != nil {
		t.Errorf("delete after unprotecting: %v\n%s", err, out)
	}
	// only admins manage rules
	if err := f.SetProtection(ctx, bob, r, protect.Rule{Pattern: "main"}); err == nil {
		t.Error("a writer set a protection rule")
	}

	// HTTP API
	var got protect.Rule
	if code := e.api("PUT", "/api/v1/repos/admin/app/protections/release/*", map[string]any{"push": "none", "required_checks": []string{"*"}}, &got); code != 200 ||
		got.Pattern != "release/*" || got.Push != "none" || !got.BlockOnChanges || got.Checks[0] != "*" {
		t.Errorf("PUT protection: %d %+v", code, got)
	}
	var list []protect.Rule
	if e.api("GET", "/api/v1/repos/admin/app/protections", nil, &list); len(list) != 1 {
		t.Errorf("GET protections: %+v", list)
	}
	asBob := &env{t: t, f: f, srv: e.srv, token: bobTok}
	if code := asBob.api("PUT", "/api/v1/repos/admin/app/protections/main", map[string]any{}, nil); code != 403 {
		t.Errorf("writer PUT protection: %d", code)
	}
	if code := e.api("DELETE", "/api/v1/repos/admin/app/protections/release/*", nil, nil); code != 204 {
		t.Errorf("DELETE protection: %d", code)
	}
}

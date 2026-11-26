package e2e

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/forge"
)

// TestUserRename: renaming moves repositories, packages and inherited secrets
// along with the user; old names stop resolving; permissions and name rules
// are enforced.
func TestUserRename(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := setup(t)
	ctx := context.Background()
	f := e.f

	r, err := f.CreateRepo(ctx, e.admin, "admin", "app", "", true)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	git(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "README"), []byte("hi"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "init")
	e.pushAs(t, work, "admin", e.token, "admin/app", "main")
	if err := f.SetOwnerSecret(ctx, e.admin, e.admin, "DEPLOY", "s3cret"); err != nil {
		t.Fatal(err)
	}
	pkg := &reg{t: t, base: e.srv.URL, user: "admin", pass: e.token}
	if resp, _ := pkg.do("PUT", "/api/packages/admin/generic/app/1.0/f", []byte("data"), nil); resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	// An empty directory left behind by a deleted user does not block the name.
	os.MkdirAll(f.Cfg.Path("repos", "andy"), 0o700)

	if err := f.RenameUser(ctx, e.admin, e.admin, "andy"); err != nil {
		t.Fatal(err)
	}
	if e.admin.Name != "andy" {
		t.Fatalf("name not updated in place: %s", e.admin.Name)
	}
	if _, err := os.Stat(f.Cfg.Path("repos", "admin")); !os.IsNotExist(err) {
		t.Error("old repository directory still exists")
	}

	// Repository: new path works over HTTP, old one is gone.
	clone := filepath.Join(t.TempDir(), "c")
	authed := strings.Replace(e.srv.URL, "http://", "http://andy:"+e.token+"@", 1)
	git(t, ".", "clone", "-q", authed+"/andy/app.git", clone)
	if b, _ := os.ReadFile(filepath.Join(clone, "README")); string(b) != "hi" {
		t.Error("cloned content wrong")
	}
	if out, err := exec.Command("git", "ls-remote", authed+"/admin/app.git").CombinedOutput(); err == nil {
		t.Errorf("old URL still works: %s", out)
	}
	repo, err := f.Store.RepoByName(ctx, "andy", "app")
	if err != nil || repo.ID != r.ID {
		t.Fatalf("repo under new name: %v %v", repo, err)
	}

	// Inherited secrets (bound to the owner ID) still decrypt.
	vals, _, err := f.JobSecrets(ctx, repo, []string{"DEPLOY"})
	if err != nil || vals["DEPLOY"] != "s3cret" {
		t.Errorf("secret after rename: %v %v", vals, err)
	}

	// Packages follow the owner.
	if resp, b := pkg.do("GET", "/api/packages/andy/generic/app/1.0/f", nil, nil); resp.StatusCode != 200 || string(b) != "data" {
		t.Errorf("package under new name: %d %q", resp.StatusCode, b)
	}
	if resp, _ := pkg.do("GET", "/api/packages/admin/generic/app/1.0/f", nil, nil); resp.StatusCode != 404 {
		t.Errorf("package under old name: %d", resp.StatusCode)
	}

	// Case-only change keeps the directory.
	if err := f.RenameUser(ctx, e.admin, e.admin, "Andy"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Git(repo).RefMap(ctx); err != nil {
		t.Errorf("repo unreadable after case-only rename: %v", err)
	}

	// Rules.
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	for _, bad := range []string{"bob", "BOB", "api", "v2", "-x", "a/b", ""} {
		if err := f.RenameUser(ctx, e.admin, e.admin, bad); err == nil {
			t.Errorf("renamed to %q", bad)
		}
	}
	if err := f.RenameUser(ctx, bob, e.admin, "x"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("bob renamed the admin: %v", err)
	}

	// Self-service over the API; site admins can rename others.
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	bobAPI := &env{t: t, f: f, srv: e.srv, token: bobTok}
	var out map[string]any
	if c := bobAPI.api("PATCH", "/api/v1/users/bob", map[string]string{"name": "robert"}, &out); c != 200 || out["name"] != "robert" {
		t.Errorf("self rename via API: %d %v", c, out)
	}
	if c := bobAPI.api("PATCH", "/api/v1/users/Andy", map[string]string{"name": "x"}, nil); c != 403 {
		t.Errorf("rename someone else via API: %d", c)
	}
	if c := e.api("PATCH", "/api/v1/users/robert", map[string]string{"name": "rob"}, nil); c != 200 {
		t.Errorf("admin renames user: %d", c)
	}

	// Organizations: owners can rename, members can't.
	org, err := f.CreateOrg(ctx, e.admin, "acme")
	if err != nil {
		t.Fatal(err)
	}
	rob, _ := f.Store.UserByName(ctx, "rob")
	f.Store.SetOrgMember(ctx, org.ID, rob.ID, "member")
	if err := f.RenameUser(ctx, rob, org, "evil"); !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("org member renamed the org: %v", err)
	}
	if _, err := f.CreateRepo(ctx, e.admin, "acme", "site", "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.RenameUser(ctx, e.admin, org, "acme-corp"); err != nil {
		t.Fatal(err)
	}
	site, err := f.Store.RepoByName(ctx, "acme-corp", "site")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.RepoPath(site)); err != nil {
		t.Errorf("org repo not moved: %v", err)
	}
}

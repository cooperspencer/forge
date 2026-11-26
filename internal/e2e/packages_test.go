package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// reg is a minimal OCI distribution client.
type reg struct {
	t          *testing.T
	base       string
	user, pass string
}

func (c *reg) do(method, path string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

// pushBlob uploads in two chunks (POST, PATCH, PATCH, PUT) and returns the status of the final PUT.
func (c *reg) pushBlob(name string, data []byte) int {
	resp, _ := c.do("POST", "/v2/"+name+"/blobs/uploads/", nil, nil)
	if resp.StatusCode != 202 {
		return resp.StatusCode
	}
	loc := resp.Header.Get("Location")
	half := len(data) / 2
	resp, _ = c.do("PATCH", loc, data[:half], map[string]string{"Content-Range": fmt.Sprintf("0-%d", half-1)})
	if resp.StatusCode != 202 || resp.Header.Get("Range") != fmt.Sprintf("0-%d", half-1) {
		c.t.Fatalf("PATCH 1: %d range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	resp, _ = c.do("PATCH", resp.Header.Get("Location"), data[half:], map[string]string{"Content-Range": fmt.Sprintf("%d-%d", half, len(data)-1)})
	if resp.StatusCode != 202 {
		c.t.Fatalf("PATCH 2: %d", resp.StatusCode)
	}
	resp, _ = c.do("PUT", resp.Header.Get("Location")+"?digest="+digestOf(data), nil, nil)
	return resp.StatusCode
}

func manifestFor(config, layer []byte) []byte {
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digestOf(config), "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": digestOf(layer), "size": len(layer)}},
	}
	b, _ := json.Marshal(m)
	return b
}

func (c *reg) pushImage(name, tag string, config, layer []byte) int {
	c.t.Helper()
	for _, b := range [][]byte{config, layer} {
		if code := c.pushBlob(name, b); code != 201 {
			return code
		}
	}
	resp, _ := c.do("PUT", "/v2/"+name+"/manifests/"+tag, manifestFor(config, layer),
		map[string]string{"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
	return resp.StatusCode
}

func TestContainerRegistry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	if _, err := f.CreateRepo(ctx, e.admin, "admin", "app", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateRepo(ctx, e.admin, "admin", "hidden", "", true); err != nil {
		t.Fatal(err)
	}
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")

	admin := &reg{t: t, base: e.srv.URL, user: "admin", pass: e.token}
	anon := &reg{t: t, base: e.srv.URL}
	asBob := &reg{t: t, base: e.srv.URL, user: "bob", pass: bobTok}

	// login handshake
	if resp, _ := anon.do("GET", "/v2/", nil, nil); resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anonymous /v2/: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, _ := admin.do("GET", "/v2/", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("authenticated /v2/: %d", resp.StatusCode)
	}

	config, layer := []byte(`{"architecture":"amd64","os":"linux"}`), bytes.Repeat([]byte("layer-data-"), 1000)
	if code := admin.pushImage("admin/app", "v1", config, layer); code != 201 {
		t.Fatalf("push: %d", code)
	}
	resp, body := anon.do("GET", "/v2/admin/app/manifests/v1", nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Content-Digest") != digestOf(body) ||
		resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("anonymous pull of public image: %d %v", resp.StatusCode, resp.Header)
	}
	mdigest := digestOf(body)
	if resp, b := anon.do("GET", "/v2/admin/app/blobs/"+digestOf(layer), nil, map[string]string{"Range": "bytes=0-9"}); resp.StatusCode != 206 || string(b) != "layer-data" {
		t.Fatalf("ranged blob GET: %d %q", resp.StatusCode, b)
	}
	if resp, b := admin.do("GET", "/v2/admin/app/tags/list", nil, nil); resp.StatusCode != 200 || !strings.Contains(string(b), `"v1"`) {
		t.Fatalf("tags: %d %s", resp.StatusCode, b)
	}

	// permissions
	if code := anon.pushImage("admin/app", "anon", config, layer); code != 401 {
		t.Errorf("anonymous push: %d", code)
	}
	if code := asBob.pushImage("admin/app", "bob", config, layer); code != 403 {
		t.Errorf("push without write access: %d", code)
	}
	if code := admin.pushImage("admin/hidden", "v1", config, layer); code != 201 {
		t.Fatalf("push private: %d", code)
	}
	if resp, _ := asBob.do("GET", "/v2/admin/hidden/manifests/v1", nil, nil); resp.StatusCode != 404 {
		t.Errorf("bob pulled a private image: %d", resp.StatusCode)
	}
	if resp, _ := anon.do("GET", "/v2/admin/hidden/manifests/v1", nil, nil); resp.StatusCode != 401 {
		t.Errorf("anonymous private pull: %d", resp.StatusCode)
	}
	// unlinked packages are private to their owner
	if code := asBob.pushImage("bob/tools", "1", config, layer); code != 201 {
		t.Fatalf("bob pushes to own namespace: %d", code)
	}
	if resp, _ := anon.do("GET", "/v2/bob/tools/manifests/1", nil, nil); resp.StatusCode != 401 {
		t.Errorf("anonymous read of unlinked package: %d", resp.StatusCode)
	}

	// Knowing a digest grants nothing: a blob from another package can't be
	// referenced or read until it is uploaded or mounted here.
	other := []byte("only in admin/hidden")
	admin.pushBlob("admin/hidden", other)
	if resp, _ := asBob.do("HEAD", "/v2/bob/tools/blobs/"+digestOf(other), nil, nil); resp.StatusCode != 404 {
		t.Errorf("blob visible through another package: %d", resp.StatusCode)
	}
	resp, _ = asBob.do("PUT", "/v2/bob/tools/manifests/stolen", manifestFor(config, other), nil)
	if resp.StatusCode != 400 {
		t.Errorf("manifest referencing a foreign blob: %d", resp.StatusCode)
	}
	resp, _ = asBob.do("POST", "/v2/bob/tools/blobs/uploads/?mount="+digestOf(other)+"&from=admin/hidden", nil, nil)
	if resp.StatusCode != 202 { // can't read the source: falls back to a normal upload
		t.Errorf("mount from unreadable package: %d", resp.StatusCode)
	}
	resp, _ = admin.do("POST", "/v2/admin/app/blobs/uploads/?mount="+digestOf(other)+"&from=admin/hidden", nil, nil)
	if resp.StatusCode != 201 {
		t.Errorf("mount from readable package: %d", resp.StatusCode)
	}

	// integrity
	resp, _ = admin.do("POST", "/v2/admin/app/blobs/uploads/?digest="+digestOf([]byte("x")), []byte("y"), nil)
	if resp.StatusCode != 400 {
		t.Errorf("digest mismatch accepted: %d", resp.StatusCode)
	}

	// delete tag, then GC removes the content once unreferenced
	if resp, _ := admin.do("DELETE", "/v2/admin/app/manifests/"+mdigest, nil, nil); resp.StatusCode != 202 {
		t.Fatalf("delete manifest: %d", resp.StatusCode)
	}
	if resp, _ := admin.do("GET", "/v2/admin/app/manifests/v1", nil, nil); resp.StatusCode != 404 {
		t.Errorf("tag survived manifest deletion: %d", resp.StatusCode)
	}
	old := forge.PackageGCGrace
	forge.PackageGCGrace = -2 * time.Second
	defer func() { forge.PackageGCGrace = old }()
	if _, err := f.GCPackages(ctx); err != nil {
		t.Fatal(err)
	}
	// The layer is still used by admin/hidden and bob/tools. `other` was only
	// ever uploaded (never referenced by a manifest), so it is collected.
	if _, ok := f.BlobAvailable(ctx, digestOf(layer)); !ok {
		t.Error("GC removed a blob still referenced by other images")
	}
	if _, ok := f.BlobAvailable(ctx, digestOf(other)); ok {
		t.Error("GC kept an unreferenced blob")
	}
	if _, ok := f.BlobAvailable(ctx, mdigest); !ok {
		// the identical manifest is still referenced by admin/hidden:v1 and bob/tools:1
		t.Error("GC removed a manifest still used elsewhere")
	}
}

func TestGenericPackages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	c := &reg{t: t, base: e.srv.URL, user: "admin", pass: e.token}
	anon := &reg{t: t, base: e.srv.URL}
	path := "/api/packages/admin/generic/app/1.2.0/app-linux-amd64"

	if resp, _ := c.do("PUT", path, []byte("binary!"), nil); resp.StatusCode != 201 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if resp, _ := c.do("PUT", path, []byte("other"), nil); resp.StatusCode != 409 {
		t.Errorf("overwrite: %d", resp.StatusCode)
	}
	if resp, b := anon.do("GET", path, nil, nil); resp.StatusCode != 200 || string(b) != "binary!" {
		t.Errorf("download: %d %q", resp.StatusCode, b)
	}
	if resp, _ := anon.do("PUT", "/api/packages/admin/generic/app/1.2.1/x", []byte("x"), nil); resp.StatusCode != 401 {
		t.Errorf("anonymous upload: %d", resp.StatusCode)
	}
	if resp, _ := c.do("PUT", "/api/packages/admin/generic/app/1.2.0/..", []byte("x"), nil); resp.StatusCode == 201 {
		t.Errorf("accepted file name ..")
	}
	var detail map[string]any
	if code := e.api("GET", "/api/v1/packages/admin/generic/app", nil, &detail); code != 200 || len(detail["files"].([]any)) != 1 {
		t.Errorf("package detail: %d %v", code, detail)
	}
	if resp, _ := c.do("DELETE", "/api/packages/admin/generic/app/1.2.0", nil, nil); resp.StatusCode != 204 {
		t.Errorf("delete version: %d", resp.StatusCode)
	}
	if resp, _ := anon.do("GET", path, nil, nil); resp.StatusCode != 404 {
		t.Errorf("deleted file still served: %d", resp.StatusCode)
	}
}

// TestJobTokenPackages: CI job tokens may push packages of their own
// repository only, never from fork PRs, and only while the job runs.
func TestJobTokenPackages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	repo, _ := f.CreateRepo(ctx, e.admin, "admin", "app", "", false)
	f.CreateRepo(ctx, e.admin, "admin", "other", "", false)
	tok, _ := f.Store.CreateRunner(ctx, "r", []string{"x"})
	rn, _ := f.Store.RunnerByToken(ctx, tok)

	start := func(fork bool) int64 {
		spec, _ := json.Marshal(ci.JobSpec{Key: "j", Name: "j", Image: "x", Fork: fork})
		if _, err := f.Store.CreateRun(ctx, &store.Run{RepoID: repo.ID, Pipeline: "p", Trigger: "push", Ref: "refs/heads/main", SHA: "abc"}, e.admin.ID,
			[]store.NewJob{{Key: "j", Name: "j", Spec: string(spec)}}); err != nil {
			t.Fatal(err)
		}
		j, err := f.Store.ClaimJob(ctx, rn.ID, []string{"x"})
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	put := func(jobID int64, path string) int {
		c := &reg{t: t, base: e.srv.URL, user: "forge-job", pass: f.JobToken(jobID)}
		resp, _ := c.do("PUT", path, []byte("data"), nil)
		return resp.StatusCode
	}

	job := start(false)
	if code := put(job, "/api/packages/admin/generic/app/1/f"); code != 201 {
		t.Errorf("job push to own repo's package: %d", code)
	}
	if code := put(job, "/api/packages/admin/generic/other/1/f"); code != 403 {
		t.Errorf("job push to another repo's package: %d", code)
	}
	f.Store.FinishJob(ctx, job, store.StatusSuccess, 0)
	if code := put(job, "/api/packages/admin/generic/app/2/f"); code != 401 {
		t.Errorf("job token after the job finished: %d", code)
	}
	forkJob := start(true)
	if code := put(forkJob, "/api/packages/admin/generic/app/3/f"); code != 403 {
		t.Errorf("fork PR job pushed a package: %d", code)
	}
}

package e2e

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestEncodedTraversal: a request authorized for one repository can't name
// another one with ".." between encoded slashes, which ServeMux doesn't
// clean.
func TestEncodedTraversal(t *testing.T) {
	e := setup(t)
	e.f.Cfg.AnonymousRead = true
	for name, private := range map[string]bool{"pub": false, "priv": true} {
		if code := e.api("POST", "/api/v1/repos", map[string]any{"name": name, "private": private}, nil); code != 201 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	host := strings.TrimPrefix(e.srv.URL, "http://")
	get := func(path string) int {
		t.Helper()
		c, err := net.Dial("tcp", host)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		// a raw request line: Go's client would normalize the path
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, host)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{
		"/admin/pub.git/info/refs%2f..%2f..%2f..%2fpriv.git/info/refs?service=git-upload-pack",
		"/admin/pub.git/objects/..%2f..%2fpriv.git/HEAD",
		"/admin/pub.git/info/lfs/objects%2f..%2f..%2f..%2f..%2fpriv.git/info/lfs/objects/batch",
		"/admin/pub.git/.%2finfo/refs?service=git-upload-pack",
	} {
		if code := get(p); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, code)
		}
	}
	if code := get("/admin/pub.git/info/refs?service=git-upload-pack"); code != 200 {
		t.Errorf("plain public clone: %d", code)
	}
}

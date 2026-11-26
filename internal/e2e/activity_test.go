package e2e

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cooperspencer/forge/internal/sshd"
)

// TestActivityGraph: a user's contributions, counted only in repositories
// the viewer can read, over the API and the CLI.
func TestActivityGraph(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	f := e.f
	bob, _ := f.Store.CreateUser(ctx, "bob", false, false)
	bobTok, _ := f.Store.CreateAccessToken(ctx, bob.ID, "t")
	asBob := &env{t: t, f: f, srv: e.srv, token: bobTok}

	pub, err := f.CreateRepo(ctx, e.admin, "admin", "pub", "", false) // 1 (repo created)
	if err != nil {
		t.Fatal(err)
	}
	priv, _ := f.CreateRepo(ctx, e.admin, "admin", "priv", "", true) // 1, private
	for i := range 3 {
		if _, err := f.CreateIssue(ctx, e.admin, pub, "issue", ""); err != nil { // 3
			t.Fatal(err, i)
		}
	}
	f.CreateIssue(ctx, e.admin, priv, "secret", "")                                // 1, private
	f.Store.RecordEvent(ctx, pub.ID, e.admin.ID, "settings", "admin set secret X") // not a contribution
	f.Store.RecordEvent(ctx, pub.ID, e.admin.ID, "pr", "PR #9 updated to abc")     // follows from a push

	total := func(as *env, user string) int {
		t.Helper()
		var out []struct{ Timestamp, Contributions int64 }
		if code := as.api("GET", "/api/v1/users/"+user+"/heatmap", nil, &out); code != 200 {
			t.Fatalf("heatmap: %d", code)
		}
		n := 0
		for _, b := range out {
			if b.Timestamp%900 != 0 {
				t.Fatalf("bucket %d isn't on a 15-minute boundary", b.Timestamp)
			}
			n += int(b.Contributions)
		}
		return n
	}
	if n := total(e, "admin"); n != 6 {
		t.Errorf("admin sees %d of their contributions, want 6", n)
	}
	if n := total(asBob, "admin"); n != 4 {
		t.Errorf("bob sees %d of admin's contributions, want 4 (public only)", n)
	}
	if n := total(asBob, "bob"); n != 0 {
		t.Errorf("bob has %d contributions, want 0", n)
	}
	if code := asBob.api("GET", "/api/v1/users/nobody/heatmap", nil, nil); code != 404 {
		t.Errorf("unknown user: %d", code)
	}

	cli := func(line ...string) (string, int) {
		var out, errb bytes.Buffer
		c := &sshd.CLI{F: f, User: bob, In: strings.NewReader(""), Out: &out, Err: &errb, Ctx: ctx}
		code := c.Run(line)
		return out.String() + errb.String(), code
	}
	out, code := cli("activity", "admin")
	if code != 0 || !strings.Contains(out, "admin: 4 contributions in the last year") || !strings.Contains(out, "Mon ") || !strings.Contains(out, "█") {
		t.Errorf("activity admin: exit %d\n%s", code, out)
	}
	out, code = cli("activity", "--weeks", "4")
	if code != 0 || !strings.Contains(out, "bob: 0 contributions in the last 4 weeks") {
		t.Errorf("activity: exit %d\n%s", code, out)
	}
	anon := &sshd.CLI{F: f, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{}, Ctx: ctx}
	if anon.Run([]string{"activity"}) == 0 {
		t.Error("anonymous activity without a user should fail")
	}
}

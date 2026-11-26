package forge

import (
	"testing"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

func TestGrantFrom(t *testing.T) {
	for _, c := range []struct{ scopes, repos, want string }{
		{"*", "", "*"},
		{"code:read,issues:write", "", "code:read issues:write"},
		{"issues code:read admin", "a/b, c/*", "code:read issues:write admin"},
		{"code:read code:write", "", "code:write"},
		{"site-admin ci:read packages:none", "", "ci:read site-admin"},
	} {
		g, err := GrantFrom(c.scopes, c.repos)
		if err != nil || g.String() != c.want {
			t.Errorf("GrantFrom(%q): %v %q, want %q", c.scopes, err, g, c.want)
		}
	}
	for _, c := range []struct{ scopes, repos string }{
		{"", ""}, {"code:admin", ""}, {"admin:write", ""}, {"repo", ""}, {"code", "noslash"}, {"code", "a/b/c"}, {"code", "*/x"}, {"code", "a/x*"}, {"*", "a/b"},
	} {
		if _, err := GrantFrom(c.scopes, c.repos); err == nil {
			t.Errorf("GrantFrom(%q, %q) accepted", c.scopes, c.repos)
		}
	}

	g, _ := GrantFrom("code:read issues:write", "Alice/App bob/*")
	switch {
	case !g.Covers("alice", "app"), !g.Covers("bob", "x"), g.Covers("alice", "other"):
		t.Error("Covers")
	case g.CoversOwner("alice"), !g.CoversOwner("bob"):
		t.Error("CoversOwner")
	case !g.Allows(store.ScopeCode, store.AccessRead), g.Allows(store.ScopeCode, store.AccessWrite), g.Allows(store.ScopeAdmin, store.AccessAdmin), g.Allows(store.ScopeCI, store.AccessRead):
		t.Error("Allows")
	case g.Cap() != store.AccessWrite:
		t.Errorf("Cap %v", g.Cap())
	}
	if ro, _ := GrantFrom("ci:read", ""); ro.Cap() != store.AccessRead {
		t.Error("read-only cap")
	}
	if a, _ := GrantFrom("admin", ""); a.Cap() != store.AccessAdmin {
		t.Error("admin cap")
	}
	var none *store.Grant // SSH sessions: no limits
	if !none.Allows(store.ScopeSite, store.AccessAdmin) || !none.Covers("x", "y") || none.Cap() != store.AccessAdmin {
		t.Error("nil grant")
	}
	admin := &store.User{IsAdmin: true, Grant: g}
	if admin.SiteAdmin() {
		t.Error("an admin's token without site-admin acts as site admin")
	}
	admin.Grant = store.FullGrant()
	if !admin.SiteAdmin() {
		t.Error("full token of an admin")
	}
}

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"never":      {},
		"":           {},
		"12h":        now.Add(12 * time.Hour),
		"30d":        now.AddDate(0, 0, 30),
		"90 days":    now.AddDate(0, 0, 90),
		"1 year":     now.AddDate(1, 0, 0),
		"6w":         now.AddDate(0, 0, 42),
		"2027-03-31": time.Date(2027, 3, 31, 23, 59, 59, 0, time.UTC),
	} {
		got, err := ParseExpiry(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-3d", "3x", "d"} {
		if _, err := ParseExpiry(bad, now); err == nil {
			t.Errorf("ParseExpiry(%q) accepted", bad)
		}
	}
}

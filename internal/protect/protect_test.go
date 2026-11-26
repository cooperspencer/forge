package protect

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRules(t *testing.T) {
	r := Rule{Pattern: " refs/heads/release/* ", Checks: []string{" ci ", "ci", "lint"}}
	if err := r.Validate(); err != nil || r.Pattern != "release/*" || r.Push != PushWrite || strings.Join(r.Checks, ",") != "ci,lint" {
		t.Errorf("validate: %v %+v", err, r)
	}
	for _, bad := range []Rule{{Pattern: ""}, {Pattern: "a b"}, {Pattern: "[x"}, {Pattern: "main", Push: "maybe"}, {Pattern: "main", Approvals: 99}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if !r.Matches("release/1.0") || r.Matches("release/1/hotfix") || r.Matches("main") {
		t.Error("pattern matching")
	}
	rules := []Rule{
		{Pattern: "*", Push: PushAdmin, Approvals: 1, AllowForce: true, AdminBypass: true, Checks: []string{"lint"}},
		{Pattern: "main", Push: PushNone, Approvals: 2, AllowDelete: true, Checks: []string{"ci"}},
	}
	e, ok := Effective(rules, "main")
	if !ok || e.Push != PushNone || e.Approvals != 2 || e.AllowForce || e.AllowDelete || e.AdminBypass || strings.Join(e.Checks, ",") != "lint,ci" {
		t.Errorf("effective: %+v", e)
	}
	if _, ok := Effective(rules[1:], "dev"); ok {
		t.Error("dev is not protected")
	}
	ff := func(old, new string) bool { return old == "a" }
	cases := []struct {
		access string
		u      Update
		want   string
	}{
		{"write", Update{"a", "b", "refs/heads/dev"}, "only repository admins"},
		{"admin", Update{"a", "b", "refs/heads/dev"}, ""}, // bypass
		{"write", Update{"a", "b", "refs/tags/v1"}, ""},   // tags aren't branches
		{"write", Update{ZeroSHA, "b", "refs/heads/main"}, "pull request"},
		{"write", Update{"x", ZeroSHA, "refs/heads/main"}, "can't be deleted"},
	}
	for _, c := range cases {
		err := CheckPush(rules, c.access, c.u, ff)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s %+v: %v, want %q", c.access, c.u, err, c.want)
		}
	}
	force := []Rule{{Pattern: "main", Push: PushWrite}}
	if err := CheckPush(force, "write", Update{"z", "b", "refs/heads/main"}, ff); err == nil || !strings.Contains(err.Error(), "force") {
		t.Errorf("force push: %v", err)
	}
}

func TestRunHook(t *testing.T) {
	var stderr bytes.Buffer
	os.Unsetenv(EnvRules)
	if RunHook(strings.NewReader("a b refs/heads/main\n"), &stderr) != 0 {
		t.Error("no rules: the hook must allow")
	}
	t.Setenv(EnvRules, `[{"pattern":"main","push":"none"}]`)
	t.Setenv(EnvAccess, "write")
	t.Setenv(EnvHint, "ssh host pr create x/y <branch>")
	if RunHook(strings.NewReader("a b refs/heads/main\na b refs/heads/dev\n"), &stderr) != 1 {
		t.Error("protected branch: the hook must reject")
	}
	if out := stderr.String(); !strings.Contains(out, "✗ main is protected") || !strings.Contains(out, "pr create x/y") {
		t.Errorf("hook output: %q", out)
	}
	if s := HookScript("/opt/my forge/it's"); !strings.Contains(s, `exec '/opt/my forge/it'\''s' hook pre-receive`) {
		t.Errorf("script quoting: %s", s)
	}
}

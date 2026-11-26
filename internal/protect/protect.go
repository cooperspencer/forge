// Package protect holds branch protection rules and their evaluation. It is
// shared by the server (merges, branch deletion) and by the pre-receive hook
// that forge installs for repositories with rules (pushes): the hook is the
// forge binary itself, run by git with the rules in its environment.
package protect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// Who may push directly to a protected branch.
const (
	PushWrite = "write" // anyone with write access
	PushAdmin = "admin" // repository admins only
	PushNone  = "none"  // nobody: changes go through pull requests
)

// Rule protects the branches matching Pattern ("main", "release/*").
type Rule struct {
	Pattern        string   `json:"pattern"`
	Push           string   `json:"push"`
	AllowForce     bool     `json:"allow_force_push"`
	AllowDelete    bool     `json:"allow_delete"`
	Approvals      int      `json:"required_approvals"`
	Checks         []string `json:"required_checks"` // pipeline names; "*" = all of them
	BlockOnChanges bool     `json:"block_on_requested_changes"`
	AdminBypass    bool     `json:"admins_bypass"`
}

// Validate normalises and checks a rule.
func (r *Rule) Validate() error {
	r.Pattern = strings.TrimPrefix(strings.TrimSpace(r.Pattern), "refs/heads/")
	if r.Pattern == "" || strings.ContainsAny(r.Pattern, " \t\n\\") || strings.HasPrefix(r.Pattern, "-") {
		return fmt.Errorf("invalid branch pattern %q", r.Pattern)
	}
	if _, err := path.Match(r.Pattern, "x"); err != nil {
		return fmt.Errorf("invalid branch pattern %q: %v", r.Pattern, err)
	}
	switch r.Push {
	case "":
		r.Push = PushWrite
	case PushWrite, PushAdmin, PushNone:
	default:
		return fmt.Errorf("push must be write, admin or none")
	}
	if r.Approvals < 0 || r.Approvals > 20 {
		return fmt.Errorf("required approvals must be between 0 and 20")
	}
	var checks []string
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			checks = append(checks, c)
		}
	}
	if seen["*"] {
		checks = []string{"*"}
	}
	r.Checks = checks
	return nil
}

// Matches reports whether the rule covers branch.
func (r Rule) Matches(branch string) bool {
	if r.Pattern == branch {
		return true
	}
	ok, _ := path.Match(r.Pattern, branch)
	return ok
}

// Summary describes a rule in a few words.
func (r Rule) Summary() string {
	var p []string
	switch r.Push {
	case PushNone:
		p = append(p, "pull requests only")
	case PushAdmin:
		p = append(p, "admins push")
	}
	if r.Approvals > 0 {
		p = append(p, fmt.Sprintf("%d approval(s)", r.Approvals))
	}
	if len(r.Checks) == 1 && r.Checks[0] == "*" {
		p = append(p, "all checks pass")
	} else if len(r.Checks) > 0 {
		p = append(p, "checks: "+strings.Join(r.Checks, ", "))
	}
	if !r.AllowForce {
		p = append(p, "no force push")
	}
	if !r.AllowDelete {
		p = append(p, "no delete")
	}
	if r.AdminBypass {
		p = append(p, "admins bypass")
	}
	if len(p) == 0 {
		return "no restrictions"
	}
	return strings.Join(p, " · ")
}

// Effective combines all rules matching branch, strictest wins.
func Effective(rules []Rule, branch string) (Rule, bool) {
	var out Rule
	found := false
	strict := map[string]int{PushWrite: 0, PushAdmin: 1, PushNone: 2}
	for _, r := range rules {
		if !r.Matches(branch) {
			continue
		}
		if !found {
			out, found = r, true
			out.Checks = append([]string(nil), r.Checks...)
			continue
		}
		if strict[r.Push] > strict[out.Push] {
			out.Push = r.Push
		}
		out.AllowForce = out.AllowForce && r.AllowForce
		out.AllowDelete = out.AllowDelete && r.AllowDelete
		out.Approvals = max(out.Approvals, r.Approvals)
		out.Checks = append(out.Checks, r.Checks...)
		out.BlockOnChanges = out.BlockOnChanges || r.BlockOnChanges
		out.AdminBypass = out.AdminBypass && r.AdminBypass
		out.Pattern += ", " + r.Pattern
	}
	if found {
		out.Validate()
	}
	return out, found
}

// ZeroSHA is the "no commit" id of a created or deleted ref.
const ZeroSHA = "0000000000000000000000000000000000000000"

// Update is one ref change of a push.
type Update struct{ Old, New, Ref string }

// CheckPush decides whether a direct push may make this change. access is
// "write" or "admin"; isAncestor reports whether old is an ancestor of new.
func CheckPush(rules []Rule, access string, u Update, isAncestor func(old, new string) bool) error {
	branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
	if !ok {
		return nil
	}
	r, ok := Effective(rules, branch)
	if !ok || (access == PushAdmin && r.AdminBypass) {
		return nil
	}
	if u.New == ZeroSHA {
		if !r.AllowDelete {
			return fmt.Errorf("%s is protected and can't be deleted", branch)
		}
		return nil
	}
	switch {
	case r.Push == PushNone:
		return fmt.Errorf("%s is protected: changes must go through a pull request", branch)
	case r.Push == PushAdmin && access != PushAdmin:
		return fmt.Errorf("%s is protected: only repository admins may push to it", branch)
	}
	if u.Old != ZeroSHA && !r.AllowForce && !isAncestor(u.Old, u.New) {
		return fmt.Errorf("%s is protected: force pushes (rewriting its history) are not allowed", branch)
	}
	return nil
}

// Hook environment, set by the server when it runs git receive-pack.
const (
	EnvRules  = "FORGE_PROTECT_RULES"
	EnvAccess = "FORGE_PROTECT_ACCESS"
	EnvHint   = "FORGE_PROTECT_HINT" // how to open a pull request instead
)

// HookEnv is the environment that makes the pre-receive hook enforce rules.
func HookEnv(rules []Rule, access, hint string) []string {
	b, _ := json.Marshal(rules)
	return []string{EnvRules + "=" + string(b), EnvAccess + "=" + access, EnvHint + "=" + hint}
}

// HookScript is the pre-receive hook: it runs the forge binary at exe.
func HookScript(exe string) string {
	return "#!/bin/sh\n# installed by forge: enforces branch protection\nexec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' hook pre-receive\n"
}

// RunHook is `forge hook pre-receive`: it reads "<old> <new> <ref>" lines and
// rejects the whole push if any change breaks a rule. Git runs it inside the
// repository with the pushed objects visible (quarantine environment).
func RunHook(stdin io.Reader, stderr io.Writer) int {
	raw, ok := os.LookupEnv(EnvRules)
	if !ok {
		return 0 // not a protected repository
	}
	var rules []Rule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		fmt.Fprintf(stderr, "forge: invalid branch protection rules: %v\n", err)
		return 1
	}
	access := os.Getenv(EnvAccess)
	isAncestor := func(old, new string) bool {
		return exec.Command("git", "merge-base", "--is-ancestor", old, new).Run() == nil
	}
	var problems []string
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		if err := CheckPush(rules, access, Update{Old: f[0], New: f[1], Ref: f[2]}, isAncestor); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if len(problems) == 0 {
		return 0
	}
	fmt.Fprintln(stderr, "")
	for _, p := range problems {
		fmt.Fprintf(stderr, "forge: ✗ %s\n", p)
	}
	if h := os.Getenv(EnvHint); h != "" {
		fmt.Fprintf(stderr, "forge:   push to another branch and open a pull request: %s\n", h)
	}
	fmt.Fprintln(stderr, "")
	return 1
}

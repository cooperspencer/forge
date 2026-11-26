package forge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

// Branch protection. Rules are enforced in three places:
//   - pushes (SSH and HTTP): git runs a pre-receive hook, the forge binary
//     itself (`forge hook pre-receive`), with the rules in its environment;
//   - pull request merges (MergePR);
//   - deleting a branch from the TUI (DeleteBranch).
// Repositories without rules are pushed to without any hook.

// installHooks writes the pre-receive hook that runs this binary.
func (f *Forge) installHooks() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	dir := f.Cfg.Path("hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "pre-receive"), []byte(protect.HookScript(exe)), 0o755)
}

// Protections lists a repository's rules (readers may see them).
func (f *Forge) Protections(ctx context.Context, actor *store.User, r *store.Repo) ([]protect.Rule, error) {
	if f.Store.AccessLevel(ctx, actor, r) < store.AccessRead {
		return nil, ErrNotFound
	}
	return f.Store.Protections(ctx, r.ID)
}

// SetProtection creates or replaces a rule (repository admins).
func (f *Forge) SetProtection(ctx context.Context, actor *store.User, r *store.Repo, rule protect.Rule) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	if err := rule.Validate(); err != nil {
		return err
	}
	if err := f.Store.SaveProtection(ctx, r.ID, rule); err != nil {
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", fmt.Sprintf("%s protected %s (%s)", actor.Name, rule.Pattern, rule.Summary()))
	return nil
}

func (f *Forge) RemoveProtection(ctx context.Context, actor *store.User, r *store.Repo, pattern string) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessAdmin {
		return ErrForbidden
	}
	if err := f.Store.DeleteProtection(ctx, r.ID, pattern); err != nil {
		return err
	}
	f.Store.RecordEvent(ctx, r.ID, actor.ID, "settings", fmt.Sprintf("%s removed the protection of %s", actor.Name, pattern))
	return nil
}

// BranchRule is the effective protection of a branch, if any.
func (f *Forge) BranchRule(ctx context.Context, r *store.Repo, branch string) (protect.Rule, bool) {
	rules, err := f.Store.Protections(ctx, r.ID)
	if err != nil {
		return protect.Rule{}, false
	}
	return protect.Effective(rules, branch)
}

func (f *Forge) protectAccess(ctx context.Context, actor *store.User, r *store.Repo) string {
	if f.Store.AccessLevel(ctx, actor, r) >= store.AccessAdmin {
		return protect.PushAdmin
	}
	return protect.PushWrite
}

// PushHook returns the git config and environment that make a push to r
// enforce its rules; both are empty for repositories without rules.
func (f *Forge) PushHook(ctx context.Context, actor *store.User, r *store.Repo) (config, env []string) {
	rules, err := f.Store.Protections(ctx, r.ID)
	if err != nil || len(rules) == 0 {
		return nil, nil
	}
	hint := fmt.Sprintf("%s pr create %s <branch>", f.Cfg.SSHCommand(), r.FullName())
	return []string{"core.hooksPath=" + f.Cfg.Path("hooks")}, protect.HookEnv(rules, f.protectAccess(ctx, actor, r), hint)
}

// DeleteBranch deletes a branch unless it is the default or protected.
func (f *Forge) DeleteBranch(ctx context.Context, actor *store.User, r *store.Repo, name string) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	if name == r.DefaultBranch {
		return fmt.Errorf("cannot delete the default branch")
	}
	if r.Mirror {
		return f.ReadOnlyError(ctx, r)
	}
	rules, _ := f.Store.Protections(ctx, r.ID)
	ref := "refs/heads/" + name
	g := f.Git(r)
	old, err := g.ResolveRef(ctx, ref)
	if err != nil {
		return err
	}
	if err := protect.CheckPush(rules, f.protectAccess(ctx, actor, r), protect.Update{Old: old, New: protect.ZeroSHA, Ref: ref}, nil); err != nil {
		return err
	}
	before, _ := g.RefMap(ctx)
	if err := g.DeleteRef(ctx, ref); err != nil {
		return err
	}
	after, _ := g.RefMap(ctx)
	f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	return nil
}

// MergeBlockers lists what keeps actor from merging pull request i under the
// base branch's protection (nothing if the branch isn't protected, or the
// actor is an admin and the rule lets admins bypass it).
func (f *Forge) MergeBlockers(ctx context.Context, actor *store.User, r *store.Repo, i *store.Issue, st *PRStatus) []string {
	if i.PR == nil {
		return nil
	}
	rule, ok := f.BranchRule(ctx, r, i.PR.BaseBranch)
	if !ok {
		return nil
	}
	admin := f.protectAccess(ctx, actor, r) == protect.PushAdmin
	if admin && rule.AdminBypass {
		return nil
	}
	var out []string
	if rule.Push == protect.PushAdmin && !admin {
		out = append(out, fmt.Sprintf("only repository admins may merge into %s", i.PR.BaseBranch))
	}
	if rule.Approvals > 0 {
		n := 0
		for _, u := range st.Approvals {
			if u != i.Author { // authors can't approve their own changes
				n++
			}
		}
		if n < rule.Approvals {
			out = append(out, fmt.Sprintf("needs %d approval(s), has %d", rule.Approvals, n))
		}
	}
	if rule.BlockOnChanges && len(st.Changes) > 0 {
		out = append(out, "changes requested by "+strings.Join(st.Changes, ", "))
	}
	if len(rule.Checks) > 0 {
		status := map[string]string{}
		for _, run := range st.Checks {
			status[run.Pipeline] = run.Status
		}
		if rule.Checks[0] == "*" {
			switch {
			case len(st.Checks) == 0:
				out = append(out, "required checks haven't run for the latest commit")
			case !st.ChecksDone:
				out = append(out, "required checks are still running")
			case !st.ChecksOK:
				out = append(out, "required checks failed")
			}
		} else {
			for _, name := range rule.Checks {
				switch s := status[name]; s {
				case store.StatusSuccess:
				case "":
					out = append(out, fmt.Sprintf("required check %q hasn't run for the latest commit", name))
				default:
					out = append(out, fmt.Sprintf("required check %q is %s", name, s))
				}
			}
		}
	}
	return out
}

package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

var forgeForbidden = forge.ErrForbidden

func diffRefs(a, b map[string]string) []forge.RefUpdate { return forge.DiffRefs(a, b) }

type settingItem struct {
	kind string // collab | secret
	name string
	id   int64
}

func (s *repoState) settingsTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Settings", List: NewList(st, Column{Title: "KIND", Width: 13}, Column{Title: "NAME", Flex: 1}, Column{Title: "VALUE", Flex: 1})}
	t.Header = func() string {
		r := s.repo
		vis := "public"
		if r.Private {
			vis = "private"
		}
		lfs := st.Faint.Render("off")
		if n, size, _ := f.Store.LFSUsage(c, r.ID); r.LFS {
			lfs = st.StatusOK.Render("on") + st.Faint.Render(fmt.Sprintf(" · %d object(s), %s", n, humanSize(size)))
		} else if n > 0 {
			lfs = st.Faint.Render(fmt.Sprintf("off · %d object(s), %s still served", n, humanSize(size)))
		}
		return fmt.Sprintf(" %s %s   %s %s   %s %s   %s %s", st.Subtle.Render("default branch"), st.Bold.Render(r.DefaultBranch),
			st.Subtle.Render("visibility"), st.Bold.Render(vis), st.Subtle.Render("owner"), st.Bold.Render(r.Owner),
			st.Subtle.Render("Git LFS"), lfs)
	}
	t.Load = func() []Row {
		var rows []Row
		ms, _ := f.Store.RepoMirrors(c, s.repo.ID)
		for _, m := range ms {
			kind := st.Accent.Render("⇡ push mirror")
			if m.Direction == forge.MirrorPull {
				kind = st.Accent.Render("⇣ pull mirror")
			}
			rows = append(rows, Row{Cols: []string{kind, st.Bold.Render(m.URL), mirrorState(st, m)}, Data: settingItem{kind: "mirror", name: m.URL, id: m.ID}})
		}
		rules, _ := f.Store.Protections(c, s.repo.ID)
		for _, pr := range rules {
			rows = append(rows, Row{Cols: []string{st.StatusRun.Render("🔒 protected"), st.Bold.Render(pr.Pattern), st.Subtle.Render(pr.Summary())},
				Data: settingItem{kind: "protection", name: pr.Pattern}})
		}
		cs, _ := f.Store.Collaborators(c, s.repo.ID)
		for _, cb := range cs {
			rows = append(rows, Row{Cols: []string{st.Accent.Render("collaborator"), cb.User.Name, cb.Role}, Data: settingItem{"collab", cb.User.Name, cb.User.ID}})
		}
		ss, _ := f.RepoSecrets(c, s.ctx.User, s.repo)
		for _, sc := range ss {
			if sc.Owner == "" {
				rows = append(rows, Row{Cols: []string{st.Label.Render("secret"), sc.Name, st.Faint.Render("•••••• (set " + ago(sc.UpdatedAt) + ")")}, Data: settingItem{"secret", sc.Name, 0}})
				continue
			}
			note := "inherited from " + sc.Owner
			if sc.Overridden {
				note += " · overridden"
			}
			rows = append(rows, Row{Cols: []string{st.Faint.Render("secret"), st.Faint.Render(sc.Name), st.Faint.Render(note)}, Data: settingItem{"inherited", sc.Owner, 0}})
		}
		t.List.Empty = "No protected branches, collaborators or secrets. Press p, c or s to add."
		return rows
	}
	t.Keys = []Key{{"e", "edit repo"}, {"M", "push mirror"}, {"y", "sync mirror"}, {"K", "deploy key"}, {"p", "protect branch"}, {"c", "collaborator"}, {"s", "secret"}, {"enter", "edit rule"}, {"d", "remove"}, {"R", "rename"}, {"X", "delete repo"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		r := s.repo
		switch k {
		case "e":
			vis := "public"
			if r.Private {
				vis = "private"
			}
			return push(NewForm(s.ctx, "Edit "+r.FullName(), []*Field{
				{Label: "Description", Value: r.Description},
				{Label: "Default branch", Value: r.DefaultBranch},
				{Label: "Visibility", Options: []string{"public", "private"}, Value: vis},
				{Label: "Git LFS", Checkbox: true, Value: fmt.Sprint(r.LFS), Placeholder: "Store large files with Git LFS",
					Hint: "turning it off refuses new uploads; existing LFS files stay downloadable"},
			}, func(v map[string]string) (tea.Cmd, error) {
				cp := *r
				cp.Description, cp.DefaultBranch, cp.Private = v["Description"], v["Default branch"], v["Visibility"] == "private"
				cp.LFS = v["Git LFS"] == "true"
				if _, err := s.git.ResolveRef(c, "refs/heads/"+cp.DefaultBranch); err != nil && !s.git.IsEmpty(c) {
					return nil, fmt.Errorf("branch %q does not exist", cp.DefaultBranch)
				}
				if err := f.UpdateRepo(c, s.ctx.User, &cp); err != nil {
					return nil, err
				}
				*r = cp
				return tea.Sequence(pop, toast("settings saved")), nil
			})), true
		case "c":
			return push(NewForm(s.ctx, "Add collaborator", []*Field{{Label: "User"}, {Label: "Role", Options: []string{"read", "write", "admin"}, Value: "write"}},
				func(v map[string]string) (tea.Cmd, error) {
					u, err := f.Store.UserByName(c, v["User"])
					if err != nil || u.IsOrg {
						return nil, fmt.Errorf("unknown user %q", v["User"])
					}
					if err := f.Store.SetCollaborator(c, r.ID, u.ID, v["Role"]); err != nil {
						return nil, err
					}
					f.Store.RecordEvent(c, r.ID, s.ctx.User.ID, "access", fmt.Sprintf("%s granted %s %s access", s.ctx.User.Name, u.Name, v["Role"]))
					return tea.Sequence(pop, toast("collaborator saved")), nil
				})), true
		case "s":
			return push(NewForm(s.ctx, "Set CI secret", []*Field{{Label: "Name", Placeholder: "DEPLOY_TOKEN"}, {Label: "Value", Secret: true, Hint: "encrypted; only jobs that list it under secrets: receive it"}},
				func(v map[string]string) (tea.Cmd, error) {
					if err := f.SetRepoSecret(c, s.ctx.User, r, v["Name"], v["Value"]); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast("secret saved")), nil
				})), true
		case "M":
			return push(mirrorForm(s.ctx, r, nil)), true
		case "y", "K":
			if row == nil || row.Data.(settingItem).kind != "mirror" {
				return toastErr(fmt.Errorf("select a mirror first")), true
			}
			it := row.Data.(settingItem)
			if k == "y" {
				return do(func() error { return f.SyncMirrorNow(c, s.ctx.User, r, it.id) }, "syncing "+it.name+"…", refreshCmd), true
			}
			m, err := f.Store.MirrorByID(c, it.id)
			if err != nil || m.SSHPub == "" {
				return toastErr(fmt.Errorf("this mirror uses https; deploy keys are for ssh remotes")), true
			}
			access := "read access"
			if m.Direction == forge.MirrorPush {
				access = "write access"
			}
			body := st.Title.Render("Deploy key for "+m.URL) + "\n\n" +
				st.Subtle.Render("Add this public key to the remote repository with "+access+"\n(GitHub: Settings › Deploy keys; GitLab: Settings › Repository › Deploy keys),\nthen press y on the mirror to sync.") +
				"\n\n" + m.SSHPub + "\n"
			return push(NewTextPage(s.ctx, "deploy key", body)), true
		case "p":
			return push(protectForm(s.ctx, r, protect.Rule{Pattern: r.DefaultBranch, BlockOnChanges: true}, false)), true
		case "enter":
			if row == nil {
				return nil, false
			}
			if it := row.Data.(settingItem); it.kind == "mirror" {
				m, err := f.Store.MirrorByID(c, it.id)
				if err != nil {
					return toastErr(err), true
				}
				return push(mirrorForm(s.ctx, r, m)), true
			}
			if it := row.Data.(settingItem); it.kind == "protection" {
				rules, _ := f.Store.Protections(c, r.ID)
				for _, pr := range rules {
					if pr.Pattern == it.name {
						return push(protectForm(s.ctx, r, pr, true)), true
					}
				}
			}
			return nil, false
		case "d":
			if row == nil {
				return nil, true
			}
			it := row.Data.(settingItem)
			if it.kind == "inherited" {
				where := "Home › Account"
				if o, err := f.Store.UserByName(c, it.name); err == nil && o.IsOrg {
					where = "Home › Orgs › " + o.Name + " › Secrets"
				}
				return toastErr(fmt.Errorf("inherited from %s; manage it in %s", it.name, where)), true
			}
			q := "Remove " + it.kind + " " + it.name + "?"
			if it.kind == "mirror" && r.Mirror {
				if m, err := f.Store.MirrorByID(c, it.id); err == nil && m.Direction == forge.MirrorPull {
					q = "Stop mirroring " + it.name + "? The repository keeps its contents and becomes a normal, writable repository."
				}
			}
			return push(NewConfirm(s.ctx, q, func() tea.Cmd {
				return do(func() error {
					switch it.kind {
					case "secret":
						return f.DeleteRepoSecret(c, s.ctx.User, r, it.name)
					case "protection":
						return f.RemoveProtection(c, s.ctx.User, r, it.name)
					case "mirror":
						err := f.RemoveMirror(c, s.ctx.User, r, it.id)
						if err == nil {
							r.Mirror = r.Mirror && func() bool { _, e := f.Store.PullMirror(c, r.ID); return e == nil }()
						}
						return err
					}
					return f.Store.RemoveCollaborator(c, r.ID, it.id)
				}, "removed", refreshCmd)
			})), true
		case "R":
			return push(NewForm(s.ctx, "Rename repository", []*Field{{Label: "New name", Value: r.Name}}, func(v map[string]string) (tea.Cmd, error) {
				if err := f.RenameRepo(c, s.ctx.User, r, v["New name"]); err != nil {
					return nil, err
				}
				s.page.title = r.FullName()
				return tea.Sequence(pop, toast("renamed to "+r.FullName())), nil
			})), true
		case "X":
			return push(NewForm(s.ctx, "Delete "+r.FullName(), []*Field{{Label: "Type the repository name to confirm", Placeholder: r.Name}},
				func(v map[string]string) (tea.Cmd, error) {
					if !confirmName(v["Type the repository name to confirm"], r.Name, r.FullName()) {
						return nil, fmt.Errorf("name does not match")
					}
					if err := f.DeleteRepo(c, s.ctx.User, r); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, pop, toast("deleted "+r.FullName())), nil
				})), true
		}
		return nil, false
	}
	return t
}

var forgeFingerprint = forge.Fingerprint

var pushChoices = []string{"everyone with write access", "admins only", "nobody: pull requests only"}
var pushValues = []string{protect.PushWrite, protect.PushAdmin, protect.PushNone}

// protectForm creates or edits a branch protection rule.
func protectForm(ctx *Ctx, r *store.Repo, rule protect.Rule, editing bool) Page {
	push := pushChoices[0]
	for i, v := range pushValues {
		if v == rule.Push {
			push = pushChoices[i]
		}
	}
	title := "Protect a branch"
	if editing {
		title = "Protection of " + rule.Pattern
	}
	return NewForm(ctx, title, []*Field{
		{Label: "Branches", Value: rule.Pattern, Hint: "a name or pattern: main, release/*"},
		{Label: "Direct pushes", Options: pushChoices, Value: push},
		{Label: "Required approvals", Value: fmt.Sprint(rule.Approvals), Hint: "to merge a pull request (the author's own doesn't count)"},
		{Label: "Required checks", Value: strings.Join(rule.Checks, ", "), Placeholder: "pipeline names, comma separated; * = all; empty = none"},
		{Label: "Block on changes", Checkbox: true, Value: fmt.Sprint(rule.BlockOnChanges), Placeholder: "No merging while a reviewer requests changes"},
		{Label: "Force pushes", Checkbox: true, Value: fmt.Sprint(rule.AllowForce), Placeholder: "Allow force pushes (rewriting history)"},
		{Label: "Deletion", Checkbox: true, Value: fmt.Sprint(rule.AllowDelete), Placeholder: "Allow deleting the branch"},
		{Label: "Admins", Checkbox: true, Value: fmt.Sprint(rule.AdminBypass), Placeholder: "Repository admins can bypass these rules"},
	}, func(v map[string]string) (tea.Cmd, error) {
		n, err := strconv.Atoi(strings.TrimSpace(v["Required approvals"]))
		if err != nil && strings.TrimSpace(v["Required approvals"]) != "" {
			return nil, fmt.Errorf("required approvals must be a number")
		}
		nr := protect.Rule{Pattern: v["Branches"], Approvals: n, Checks: strings.Split(v["Required checks"], ","),
			BlockOnChanges: v["Block on changes"] == "true", AllowForce: v["Force pushes"] == "true",
			AllowDelete: v["Deletion"] == "true", AdminBypass: v["Admins"] == "true"}
		for i, ch := range pushChoices {
			if ch == v["Direct pushes"] {
				nr.Push = pushValues[i]
			}
		}
		if err := nr.Validate(); err != nil {
			return nil, err
		}
		if err := ctx.F.SetProtection(ctx.Ctx, ctx.User, r, nr); err != nil {
			return nil, err
		}
		if editing && nr.Pattern != rule.Pattern {
			ctx.F.RemoveProtection(ctx.Ctx, ctx.User, r, rule.Pattern)
		}
		return tea.Sequence(pop, toast("protected "+nr.Pattern)), nil
	})
}

func mirrorState(st *Styles, m *store.Mirror) string {
	every := "after every push"
	if m.Interval > 0 {
		every = "every " + forge.FormatInterval(m.Interval)
		if m.Direction == forge.MirrorPush {
			every = "after pushes and " + every
		}
	}
	switch {
	case m.LastError != "":
		return st.StatusFail.Render("✗ " + m.LastError)
	case m.LastSync.IsZero() && m.SSHPub != "" && m.LastTry.IsZero() && m.NextSync.IsZero():
		return st.StatusRun.Render("add the deploy key (K), then sync (y)")
	case m.LastSync.IsZero():
		return st.StatusRun.Render("waiting for the first sync")
	}
	return st.StatusOK.Render("✓ synced "+ago(m.LastSync)) + st.Faint.Render(" · "+every)
}

// mirrorForm adds a push mirror (m nil) or edits a mirror.
func mirrorForm(ctx *Ctx, r *store.Repo, m *store.Mirror) Page {
	title, url, user, iv, runCI := "Add a push mirror", "", "", "0", false
	choices := append([]string{"0"}, intervalChoices...)
	hint := "0 = only after pushes"
	if m != nil {
		title, url, user, runCI = "Mirror "+m.URL, m.URL, m.Username, m.RunCI
		iv = "0"
		for _, c := range intervalChoices {
			if d, _ := forge.ParseInterval(c); d == m.Interval {
				iv = c
			}
		}
		if m.Direction == forge.MirrorPull {
			choices, hint = intervalChoices, ""
		}
	}
	fields := []*Field{
		{Label: "Remote URL", Value: url, Placeholder: "https://github.com/owner/repo.git or git@github.com:owner/repo.git",
			Hint: "pushes branches and tags, and deletes remote branches forge doesn't have"},
		{Label: "Username", Value: user, Placeholder: "https remotes"},
		{Label: "Password or token", Secret: true, Placeholder: "https remotes; leave empty to keep the current one"},
		{Label: "Sync every", Options: choices, Value: iv, Hint: hint},
	}
	pull := m != nil && m.Direction == forge.MirrorPull
	if pull {
		fields[0].Hint = ""
		fields = append(fields, &Field{Label: "Pipelines", Checkbox: true, Value: fmt.Sprint(runCI), Placeholder: "Run pipelines for mirrored commits"})
	}
	return NewForm(ctx, title, fields, func(v map[string]string) (tea.Cmd, error) {
		d, _ := forge.ParseInterval(v["Sync every"])
		o := forge.MirrorOptions{URL: v["Remote URL"], Username: v["Username"], Password: v["Password or token"], Interval: d, RunCI: v["Pipelines"] == "true"}
		if m == nil {
			nm, err := ctx.F.AddPushMirror(ctx.Ctx, ctx.User, r, o)
			if err != nil {
				return nil, err
			}
			if nm.SSHPub != "" {
				return tea.Sequence(pop, toast("added; now add its deploy key to the remote (K)")), nil
			}
			return tea.Sequence(pop, toast("push mirror added")), nil
		}
		if err := ctx.F.UpdateMirror(ctx.Ctx, ctx.User, r, m.ID, o); err != nil {
			return nil, err
		}
		return tea.Sequence(pop, toast("mirror saved, syncing…")), nil
	})
}

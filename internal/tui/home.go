package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

func NewHome(ctx *Ctx) Page {
	st := ctx.St
	f, c := ctx.F, ctx.Ctx
	var tabs []*Tab

	// --- repositories
	repos := &Tab{Name: "Repositories", List: NewList(st,
		Column{Title: "REPOSITORY", Flex: 2}, Column{Title: "DESCRIPTION", Flex: 3}, Column{Title: "", Width: 8}, Column{Title: "UPDATED", Width: 10, Right: true})}
	repos.List.Empty = "No repositories yet. Press n to create one, or push to an empty one."
	repos.Load = func() []Row {
		rs, _ := f.Store.VisibleRepos(c, ctx.User)
		var rows []Row
		for _, r := range rs {
			vis := st.Faint.Render("public")
			if r.Private {
				vis = st.StatusRun.Render("private")
			}
			name := st.Bold.Render(r.FullName())
			if r.ForkOf != 0 {
				name += st.Faint.Render(" ⑂")
			}
			if r.Mirror {
				name += st.Accent.Render(" ⇣")
			}
			rows = append(rows, Row{Cols: []string{name, st.Subtle.Render(r.Description), vis, st.Faint.Render(ago(r.UpdatedAt))},
				Filter: r.FullName() + " " + r.Description, Data: r})
		}
		return rows
	}
	repos.Keys = []Key{{"enter", "open"}, {"n", "new repo"}, {"m", "new mirror"}}
	repos.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "enter":
			if row != nil {
				return push(NewRepoPage(ctx, row.Data.(*store.Repo))), true
			}
		case "n":
			return push(newRepoForm(ctx)), true
		case "m":
			return push(newMirrorForm(ctx)), true
		}
		return nil, false
	}
	tabs = append(tabs, repos)

	// --- activity feed
	activity := &Tab{Name: "Activity", List: NewList(st, Column{Title: "REPOSITORY", Width: 24}, Column{Title: "EVENT", Flex: 1}, Column{Title: "WHEN", Width: 10, Right: true})}
	activity.Load = func() []Row {
		evs, _ := f.Store.Feed(c, ctx.User, 200)
		return eventRows(st, evs)
	}
	activity.Keys = []Key{{"enter", "open repo"}}
	activity.Handle = openEventRepo(ctx)
	tabs = append(tabs, activity)

	if ctx.User != nil {
		notif := &Tab{Name: "Inbox", List: NewList(st, Column{Title: "", Width: 1}, Column{Title: "REPOSITORY", Width: 24}, Column{Title: "EVENT", Flex: 1}, Column{Title: "WHEN", Width: 10, Right: true})}
		notif.List.Empty = "No notifications. Watch a repository (w) to get notified about its activity."
		notif.Load = func() []Row {
			evs, _ := f.Store.Notifications(c, ctx.User.ID, 200)
			var rows []Row
			for _, e := range evs {
				dot := " "
				if !e.Read {
					dot = st.Label.Render("●")
				}
				rows = append(rows, Row{Cols: []string{dot, st.Accent.Render(e.Repo), e.Summary, st.Faint.Render(ago(e.CreatedAt))}, Data: e})
			}
			return rows
		}
		notif.Keys = []Key{{"enter", "open repo"}, {"m", "mark all read"}}
		openRepo := openEventRepo(ctx)
		notif.Handle = func(k string, row *Row) (tea.Cmd, bool) {
			if k == "m" {
				return do(func() error { return f.Store.MarkNotificationsRead(c, ctx.User.ID) }, "marked as read", refreshCmd), true
			}
			return openRepo(k, row)
		}
		tabs = append(tabs, notif)
		tabs = append(tabs, orgsTab(ctx), accountTab(ctx))
	}
	if ctx.User != nil && ctx.User.IsAdmin {
		tabs = append(tabs, runnersTab(ctx), usersTab(ctx))
	}
	if f.Cfg.Packages.Enabled && len(tabs) >= 2 {
		pk := packagesTab(ctx, "No packages yet. Push an image or upload a file — see `ssh host help package`.", func() []*store.Package {
			ps, _ := f.VisiblePackages(c, ctx.pkgActor(), nil)
			return ps
		})
		tabs = append(tabs[:2], append([]*Tab{pk}, tabs[2:]...)...)
	}
	// code search across every repository, right after the repository list
	search := newSearchPane(ctx, nil, false)
	tabs = append(tabs[:1], append([]*Tab{{Name: "Search", Custom: search}}, tabs[1:]...)...)
	p := NewTabPage(ctx, "Home", tabs)
	p.global = func(k string) (tea.Cmd, bool) {
		if k == "S" {
			p.t.Active = 1
			p.SetSize(p.w, p.h)
			return search.focusInput(), true
		}
		return nil, false
	}
	p.globalKeys = []Key{{"S", "search code"}}
	p.top = func() string {
		if ctx.User == nil {
			return st.Subtle.Render(" Browsing anonymously. Register an SSH key to collaborate.")
		}
		return ""
	}
	if ctx.User != nil {
		p.top = nil
	}
	return p
}

func refreshCmd() tea.Msg { return refreshMsg{"local"} }

func eventRows(st *Styles, evs []store.Event) []Row {
	var rows []Row
	for _, e := range evs {
		rows = append(rows, Row{Cols: []string{st.Accent.Render(e.Repo), e.Summary, st.Faint.Render(ago(e.CreatedAt))}, Data: e})
	}
	return rows
}

func openEventRepo(ctx *Ctx) func(string, *Row) (tea.Cmd, bool) {
	return func(k string, row *Row) (tea.Cmd, bool) {
		if k != "enter" || row == nil {
			return nil, false
		}
		e := row.Data.(store.Event)
		r, _, err := ctx.F.ResolveRepo(ctx.Ctx, ctx.User, e.Repo, store.AccessRead)
		if err != nil {
			return toastErr(err), true
		}
		return push(NewRepoPage(ctx, r)), true
	}
}

func newRepoForm(ctx *Ctx) Page {
	if ctx.User == nil {
		return NewConfirm(ctx, "You need an account to create repositories.", func() tea.Cmd { return nil })
	}
	owners := []string{ctx.User.Name}
	users, _ := ctx.F.Store.ListUsers(ctx.Ctx)
	for _, u := range users {
		if u.IsOrg && ctx.F.Store.CanCreateIn(ctx.Ctx, ctx.User, u) {
			owners = append(owners, u.Name)
		}
	}
	return NewForm(ctx, "New repository", []*Field{
		{Label: "Owner", Options: owners},
		{Label: "Name", Placeholder: "my-project"},
		{Label: "Description", Placeholder: "optional"},
		{Label: "Visibility", Options: []string{"public", "private"}},
		{Label: "Git LFS", Checkbox: true, Placeholder: "Store large files with Git LFS", Hint: "can be changed later in Settings"},
	}, func(v map[string]string) (tea.Cmd, error) {
		r, err := ctx.F.CreateRepoWith(ctx.Ctx, ctx.User, v["Owner"], v["Name"], v["Description"],
			forge.RepoOptions{Private: v["Visibility"] == "private", LFS: v["Git LFS"] == "true"})
		if err != nil {
			return nil, err
		}
		return tea.Sequence(pop, toast("created "+r.FullName()), push(NewRepoPage(ctx, r))), nil
	})
}

var intervalChoices = []string{"10m", "1h", "8h", "1d"}

func newMirrorForm(ctx *Ctx) Page {
	if ctx.User == nil {
		return NewConfirm(ctx, "You need an account to create repositories.", func() tea.Cmd { return nil })
	}
	owners := []string{ctx.User.Name}
	users, _ := ctx.F.Store.ListUsers(ctx.Ctx)
	for _, u := range users {
		if u.IsOrg && ctx.F.Store.CanCreateIn(ctx.Ctx, ctx.User, u) {
			owners = append(owners, u.Name)
		}
	}
	return NewForm(ctx, "New mirror of a remote repository", []*Field{
		{Label: "Remote URL", Placeholder: "https://github.com/owner/repo.git or git@github.com:owner/repo.git"},
		{Label: "Owner", Options: owners},
		{Label: "Name", Placeholder: "default: the remote's name"},
		{Label: "Username", Placeholder: "for private remotes (https)"},
		{Label: "Password or token", Secret: true, Placeholder: "for private remotes (https); stored encrypted"},
		{Label: "Sync every", Options: intervalChoices, Value: "8h"},
		{Label: "Visibility", Options: []string{"public", "private"}},
		{Label: "Pipelines", Checkbox: true, Placeholder: "Run pipelines for mirrored commits", Hint: "off: the original's CI is enough"},
	}, func(v map[string]string) (tea.Cmd, error) {
		iv, _ := forge.ParseInterval(v["Sync every"])
		r, err := ctx.F.CreatePullMirror(ctx.Ctx, ctx.User, v["Owner"], v["Name"], "", v["Visibility"] == "private", forge.MirrorOptions{
			URL: v["Remote URL"], Username: v["Username"], Password: v["Password or token"], Interval: iv, RunCI: v["Pipelines"] == "true"})
		if err != nil {
			return nil, err
		}
		return tea.Sequence(pop, toast("mirroring into "+r.FullName()+"…"), push(NewRepoPage(ctx, r))), nil
	})
}

// ---------------------------------------------------------------- account

type accountItem struct {
	kind string // key | token | secret
	id   int64
	name string
}

func accountTab(ctx *Ctx) *Tab {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Account", List: NewList(st, Column{Title: "TYPE", Width: 8}, Column{Title: "NAME", Flex: 1}, Column{Title: "DETAIL", Flex: 2}, Column{Title: "CREATED", Width: 10, Right: true})}
	t.Header = func() string {
		u := ctx.User
		return fmt.Sprintf(" %s %s  %s %s  %s %s\n", st.Subtle.Render("user"), st.Bold.Render(u.Name),
			st.Subtle.Render("name"), orDash(u.FullName), st.Subtle.Render("email"), orDash(u.Email))
	}
	t.Load = func() []Row {
		var rows []Row
		keys, _ := f.Store.SSHKeys(c, ctx.User.ID)
		for _, k := range keys {
			rows = append(rows, Row{Cols: []string{st.Accent.Render("ssh key"), k.Title, st.Faint.Render(k.Fingerprint), st.Faint.Render(ago(k.CreatedAt))}, Data: accountItem{kind: "key", id: k.ID}})
		}
		toks, _ := f.Store.AccessTokens(c, ctx.User.ID)
		for _, tk := range toks {
			rows = append(rows, Row{Cols: tokenRowCols(st, tk), Data: accountItem{kind: "token", id: tk.ID}})
		}
		ss, _ := f.OwnerSecrets(c, ctx.User, ctx.User)
		for _, sc := range ss {
			rows = append(rows, Row{Cols: []string{st.StatusWait.Render("secret"), sc.Name, st.Faint.Render("•••••• inherited by all your repositories"), st.Faint.Render(ago(sc.UpdatedAt))}, Data: accountItem{"secret", 0, sc.Name}})
		}
		return rows
	}
	t.Keys = []Key{{"a", "add ssh key"}, {"t", "new API token"}, {"s", "CI secret"}, {"e", "edit profile"}, {"R", "rename"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "a":
			return push(NewForm(ctx, "Add SSH key", []*Field{{Label: "Public key", Placeholder: "ssh-ed25519 AAAA… comment"}},
				func(v map[string]string) (tea.Cmd, error) {
					if err := f.AddKey(c, ctx.User, v["Public key"]); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast("key added")), nil
				})), true
		case "t":
			return push(newTokenForm(ctx)), true
		case "s":
			return push(secretForm(ctx, "Set CI secret for all your repositories", func(name, value string) error {
				return f.SetOwnerSecret(c, ctx.User, ctx.User, name, value)
			})), true
		case "R":
			return push(renameForm(ctx, ctx.User, pop)), true
		case "e":
			u := ctx.User
			return push(NewForm(ctx, "Edit profile", []*Field{{Label: "Full name", Value: u.FullName}, {Label: "Email", Value: u.Email, Hint: "used for merge commits"}},
				func(v map[string]string) (tea.Cmd, error) {
					u.FullName, u.Email = v["Full name"], v["Email"]
					if err := f.Store.UpdateUser(c, u); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast("profile updated")), nil
				})), true
		case "d":
			if row == nil {
				return nil, true
			}
			it := row.Data.(accountItem)
			return push(NewConfirm(ctx, "Delete this "+it.kind+"?", func() tea.Cmd {
				return do(func() error {
					switch it.kind {
					case "key":
						return f.Store.DeleteSSHKey(c, ctx.User.ID, it.id)
					case "secret":
						return f.DeleteOwnerSecret(c, ctx.User, ctx.User, it.name)
					}
					return f.Store.DeleteAccessToken(c, ctx.User.ID, it.id)
				}, "deleted", refreshCmd)
			})), true
		}
		return nil, false
	}
	return t
}

// renameForm renames a user or organization; done closes the form (and
// anything showing the old name).
func renameForm(ctx *Ctx, u *store.User, done tea.Cmd) Page {
	what := "your account"
	if u.IsOrg {
		what = "organization " + u.Name
	} else if u.ID != ctx.User.ID {
		what = "user " + u.Name
	}
	return NewForm(ctx, "Rename "+what, []*Field{{Label: "New name", Value: u.Name,
		Hint: "clone URLs, git remotes and image names under " + u.Name + "/ change; old ones stop working"}},
		func(v map[string]string) (tea.Cmd, error) {
			old := u.Name
			if err := ctx.F.RenameUser(ctx.Ctx, ctx.User, u, v["New name"]); err != nil {
				return nil, err
			}
			if u.Name == old {
				return pop, nil
			}
			if u.ID == ctx.User.ID {
				ctx.User.Name = u.Name // renamed yourself from the Users tab
			}
			return tea.Sequence(done, toast("renamed "+old+" → "+u.Name), refreshCmd), nil
		})
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------------------------------------------------------------- orgs

func orgsTab(ctx *Ctx) *Tab {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Orgs", List: NewList(st, Column{Title: "ORGANIZATION", Flex: 1}, Column{Title: "YOUR ROLE", Width: 10}, Column{Title: "MEMBERS", Width: 8, Right: true})}
	t.List.Empty = "You are not a member of any organization. Press n to create one."
	t.Load = func() []Row {
		users, _ := f.Store.ListUsers(c)
		var rows []Row
		for _, u := range users {
			if !u.IsOrg {
				continue
			}
			role, _ := f.Store.OrgRole(c, u.ID, ctx.User.ID)
			if role == "" && !ctx.User.IsAdmin {
				continue
			}
			ms, _ := f.Store.OrgMembers(c, u.ID)
			rows = append(rows, Row{Cols: []string{st.Bold.Render(u.Name), orDash(role), fmt.Sprint(len(ms))}, Data: u})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "members"}, {"n", "new org"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "n":
			return push(NewForm(ctx, "New organization", []*Field{{Label: "Name"}}, func(v map[string]string) (tea.Cmd, error) {
				if _, err := f.CreateOrg(c, ctx.User, v["Name"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("organization created")), nil
			})), true
		case "enter":
			if row != nil {
				return push(newOrgPage(ctx, row.Data.(*store.User))), true
			}
		}
		return nil, false
	}
	return t
}

func newOrgPage(ctx *Ctx, org *store.User) Page {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Members", List: NewList(st, Column{Title: "USER", Flex: 1}, Column{Title: "ROLE", Width: 10})}
	t.Load = func() []Row {
		ms, _ := f.Store.OrgMembers(c, org.ID)
		var rows []Row
		for _, m := range ms {
			rows = append(rows, Row{Cols: []string{m.User.Name, m.Role}, Data: m})
		}
		return rows
	}
	t.Keys = []Key{{"a", "add/update member"}, {"d", "remove"}, {"R", "rename org"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if !f.CanManageOrg(c, ctx.User, org) && (k == "a" || k == "d" || k == "R") {
			return toastErr(fmt.Errorf("only org owners can manage the organization")), true
		}
		switch k {
		case "R":
			// the page title shows the old name: close it after renaming
			return push(renameForm(ctx, org, tea.Sequence(pop, pop))), true
		case "a":
			return push(NewForm(ctx, "Add member to "+org.Name, []*Field{{Label: "User"}, {Label: "Role", Options: []string{"member", "owner"}}},
				func(v map[string]string) (tea.Cmd, error) {
					u, err := f.Store.UserByName(c, v["User"])
					if err != nil || u.IsOrg {
						return nil, fmt.Errorf("unknown user %q", v["User"])
					}
					if err := f.Store.SetOrgMember(c, org.ID, u.ID, v["Role"]); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast("member saved")), nil
				})), true
		case "d":
			if row != nil {
				m := row.Data.(store.OrgMember)
				return push(NewConfirm(ctx, "Remove "+m.User.Name+" from "+org.Name+"?", func() tea.Cmd {
					return do(func() error { return f.Store.RemoveOrgMember(c, org.ID, m.User.ID) }, "removed", refreshCmd)
				})), true
			}
		}
		return nil, false
	}
	return NewTabPage(ctx, org.Name, []*Tab{t, ownerSecretsTab(ctx, org)})
}

// ownerSecretsTab manages secrets of an organization, inherited by all of its
// repositories.
func ownerSecretsTab(ctx *Ctx, owner *store.User) *Tab {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Secrets", List: NewList(st, Column{Title: "NAME", Flex: 1}, Column{Title: "VALUE", Flex: 1}, Column{Title: "UPDATED", Width: 10, Right: true})}
	t.Header = func() string {
		return " " + st.Subtle.Render("CI secrets available to every repository of "+owner.Name+" (a repository secret with the same name wins)")
	}
	t.Load = func() []Row {
		ss, err := f.OwnerSecrets(c, ctx.User, owner)
		if err != nil {
			t.List.Empty = "Only " + owner.Name + "'s owners can view and manage its secrets."
			return nil
		}
		t.List.Empty = "No secrets. Press s to add one."
		var rows []Row
		for _, sc := range ss {
			rows = append(rows, Row{Cols: []string{sc.Name, st.Faint.Render("••••••"), st.Faint.Render(ago(sc.UpdatedAt))}, Data: sc.Name})
		}
		return rows
	}
	t.Keys = []Key{{"s", "set secret"}, {"d", "remove"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "s":
			return push(secretForm(ctx, "Set "+owner.Name+" secret", func(name, value string) error {
				return f.SetOwnerSecret(c, ctx.User, owner, name, value)
			})), true
		case "d":
			if row != nil {
				name := row.Data.(string)
				return push(NewConfirm(ctx, "Remove secret "+name+" from "+owner.Name+"?", func() tea.Cmd {
					return do(func() error { return f.DeleteOwnerSecret(c, ctx.User, owner, name) }, "removed", refreshCmd)
				})), true
			}
		}
		return nil, false
	}
	return t
}

func secretForm(ctx *Ctx, title string, save func(name, value string) error) Page {
	return NewForm(ctx, title, []*Field{
		{Label: "Name", Placeholder: "DEPLOY_TOKEN", Hint: "letters, digits, _ (upper-cased)"},
		{Label: "Value", Secret: true, Hint: "encrypted; only jobs that list it under secrets: receive it"},
	}, func(v map[string]string) (tea.Cmd, error) {
		if err := save(v["Name"], v["Value"]); err != nil {
			return nil, err
		}
		return tea.Sequence(pop, toast("secret saved")), nil
	})
}

// ---------------------------------------------------------------- admin

func runnersTab(ctx *Ctx) *Tab {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Runners", List: NewList(st, Column{Title: "", Width: 1}, Column{Title: "RUNNER", Flex: 1}, Column{Title: "LABELS", Flex: 1},
		Column{Title: "VERSION", Width: 10}, Column{Title: "STATE", Width: 9}, Column{Title: "LAST SEEN", Width: 10, Right: true})}
	t.List.Empty = "No runners registered. Press n to create one, then run `forge runner` on a Docker host."
	t.Load = func() []Row {
		rs, _ := f.Store.Runners(c)
		var rows []Row
		for _, r := range rs {
			dot, state := st.StatusFail.Render("●"), st.Faint.Render("offline")
			if r.Online() {
				dot, state = st.StatusOK.Render("●"), st.StatusOK.Render("online")
			}
			if !r.Enabled {
				state = st.StatusRun.Render("disabled")
			}
			rows = append(rows, Row{Cols: []string{dot, st.Bold.Render(r.Name), strings.Join(r.Labels, ","), orDash(r.Version), state, st.Faint.Render(ago(r.LastSeen))}, Data: r})
		}
		return rows
	}
	t.Keys = []Key{{"n", "register runner"}, {"e", "enable/disable"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "n":
			return push(NewForm(ctx, "Register runner", []*Field{{Label: "Name", Placeholder: "builder-1"}, {Label: "Labels", Placeholder: "docker,linux,amd64", Hint: "comma separated"}},
				func(v map[string]string) (tea.Cmd, error) {
					var labels []string
					for _, l := range strings.Split(v["Labels"], ",") {
						if l = strings.TrimSpace(l); l != "" {
							labels = append(labels, l)
						}
					}
					tok, err := f.Store.CreateRunner(c, v["Name"], labels)
					if err != nil {
						return nil, err
					}
					body := st.Title.Render("Runner registered") + "\n\nStart it on any host with Docker:\n\n  " +
						st.Accent.Render(fmt.Sprintf("forge runner --server %s --token %s", f.Cfg.HTTP.PublicURL, tok)) +
						"\n\n" + st.Subtle.Render("The token is shown only once.")
					return replace(NewTextPage(ctx, "Runner token", body)), nil
				})), true
		case "e":
			if row != nil {
				r := row.Data.(*store.Runner)
				return do(func() error { return f.Store.SetRunnerEnabled(c, r.ID, !r.Enabled) }, "updated", refreshCmd), true
			}
		case "d":
			if row != nil {
				r := row.Data.(*store.Runner)
				return push(NewConfirm(ctx, "Delete runner "+r.Name+"?", func() tea.Cmd {
					return do(func() error { return f.Store.DeleteRunner(c, r.ID) }, "runner deleted", refreshCmd)
				})), true
			}
		}
		return nil, false
	}
	return t
}

func usersTab(ctx *Ctx) *Tab {
	st, f, c := ctx.St, ctx.F, ctx.Ctx
	t := &Tab{Name: "Users", List: NewList(st, Column{Title: "NAME", Flex: 1}, Column{Title: "TYPE", Width: 6}, Column{Title: "ROLE", Width: 6}, Column{Title: "KEYS", Width: 5, Right: true}, Column{Title: "CREATED", Width: 10, Right: true})}
	t.Load = func() []Row {
		us, _ := f.Store.ListUsers(c)
		var rows []Row
		for _, u := range us {
			typ, role := "user", ""
			if u.IsOrg {
				typ = "org"
			}
			if u.IsAdmin {
				role = st.Label.Render("admin")
			}
			keys, _ := f.Store.SSHKeys(c, u.ID)
			rows = append(rows, Row{Cols: []string{st.Bold.Render(u.Name), typ, role, fmt.Sprint(len(keys)), st.Faint.Render(ago(u.CreatedAt))}, Data: u})
		}
		return rows
	}
	t.Keys = []Key{{"n", "new user"}, {"k", "add key"}, {"a", "toggle admin"}, {"R", "rename"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "n":
			return push(NewForm(ctx, "New user", []*Field{{Label: "Name"}, {Label: "SSH public key", Placeholder: "ssh-ed25519 AAAA…"}, {Label: "Role", Options: []string{"user", "admin"}}},
				func(v map[string]string) (tea.Cmd, error) {
					if _, err := f.AdminCreateUser(c, ctx.User, v["Name"], v["SSH public key"], v["Role"] == "admin"); err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast("user created")), nil
				})), true
		}
		if row == nil {
			return nil, false
		}
		u := row.Data.(*store.User)
		switch k {
		case "R":
			return push(renameForm(ctx, u, pop)), true
		case "k":
			return push(NewForm(ctx, "Add key for "+u.Name, []*Field{{Label: "SSH public key"}}, func(v map[string]string) (tea.Cmd, error) {
				if err := f.AddKey(c, u, v["SSH public key"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("key added")), nil
			})), true
		case "a":
			if u.ID == ctx.User.ID {
				return toastErr(fmt.Errorf("you cannot change your own admin flag")), true
			}
			u.IsAdmin = !u.IsAdmin
			return do(func() error { return f.Store.UpdateUser(c, u) }, "updated", refreshCmd), true
		case "d":
			if u.ID == ctx.User.ID {
				return toastErr(fmt.Errorf("you cannot delete yourself")), true
			}
			return push(NewConfirm(ctx, "Delete "+u.Name+" and all their repositories?", func() tea.Cmd {
				return do(func() error {
					repos, _ := f.Store.VisibleRepos(c, ctx.User)
					for _, r := range repos {
						if r.OwnerID == u.ID {
							if err := f.DeleteRepo(c, ctx.User, r); err != nil {
								return err
							}
						}
					}
					return f.Store.DeleteUser(c, u.ID)
				}, "deleted", refreshCmd)
			})), true
		}
		return nil, false
	}
	return t
}

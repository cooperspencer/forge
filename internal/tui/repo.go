package tui

import (
	"fmt"
	"path"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/ci"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

type repoState struct {
	ctx  *Ctx
	repo *store.Repo
	git  *gitx.Repo
	ref  string // branch / tag / sha being browsed
	path string // directory in Code tab
	page *TabPage

	issueState string // open | closed | all
	prState    string

	readme readmeCache // README of the directory shown in the Code tab
}

func NewRepoPage(ctx *Ctx, r *store.Repo) Page {
	s := &repoState{ctx: ctx, repo: r, git: ctx.F.Git(r), ref: r.DefaultBranch, issueState: "open", prState: "open"}
	tabs := []*Tab{s.codeTab(), s.commitsTab(), s.branchesTab(), s.tagsTab(), s.issuesTab(false), s.boardTab(), s.issuesTab(true), s.wikiTab(), s.pipelinesTab(), s.releasesTab()}
	if ctx.F.Cfg.Packages.Enabled {
		tabs = append(tabs, packagesTab(ctx, "No packages. Images pushed to "+ctx.F.Cfg.RegistryHost()+"/"+strings.ToLower(r.FullName())+" show up here.",
			func() []*store.Package { ps, _ := ctx.F.RepoPackages(ctx.Ctx, r); return ps }))
	}
	tabs = append(tabs, s.activityTab())
	if ctx.Access(r) >= store.AccessAdmin {
		tabs = append(tabs, s.settingsTab())
	}
	p := NewTabPage(ctx, r.FullName(), tabs)
	s.page = p
	p.top = s.summary
	p.global = s.globalKeys
	p.globalKeys = []Key{{"w", "watch"}, {"f", "fork"}, {"S", "search code"}}
	return p
}

func (s *repoState) summary() string {
	st := s.ctx.St
	r := s.repo
	vis := st.Faint.Render("public")
	if r.Private {
		vis = st.StatusRun.Render("private")
	}
	line1 := " " + st.Title.Render(r.FullName()) + "  " + vis + "  " + st.Subtle.Render(s.ctx.Access(r).String()+" access")
	if r.LFS {
		line1 += "  " + st.Accent.Render("LFS")
	}
	if r.ForkOfName != "" {
		line1 += st.Faint.Render("  ⑂ fork of " + r.ForkOfName)
	}
	if s.ctx.User != nil && s.ctx.F.Store.IsWatching(s.ctx.Ctx, s.ctx.User.ID, r.ID) {
		line1 += "  " + st.Label.Render("◉ watching")
	}
	desc := r.Description
	if desc == "" {
		desc = "No description"
	}
	line2 := " " + st.Subtle.Render(desc)
	if r.Mirror {
		line2 += "   " + mirrorStatus(st, s.ctx, r)
	}
	line3 := " " + st.Faint.Render("clone ") + st.Accent.Render(s.ctx.F.Cfg.SSHCloneURL(r.FullName())) + st.Faint.Render("  or  ") + st.Accent.Render(s.ctx.F.Cfg.HTTPCloneURL(r.FullName()))
	return line1 + "\n" + line2 + "\n" + line3
}

func (s *repoState) globalKeys(k string) (tea.Cmd, bool) {
	f, c, u := s.ctx.F, s.ctx.Ctx, s.ctx.User
	switch k {
	case "S":
		return push(NewSearchPage(s.ctx, s.repo)), true
	case "w":
		if u == nil {
			return toastErr(fmt.Errorf("sign in to watch repositories")), true
		}
		on := !f.Store.IsWatching(c, u.ID, s.repo.ID)
		msg := "watching " + s.repo.FullName()
		if !on {
			msg = "stopped watching"
		}
		return do(func() error { return f.Store.Watch(c, u.ID, s.repo.ID, on) }, msg), true
	case "f":
		if u == nil {
			return toastErr(fmt.Errorf("sign in to fork")), true
		}
		return push(NewConfirm(s.ctx, "Fork "+s.repo.FullName()+" to "+u.Name+"/"+s.repo.Name+"?", func() tea.Cmd {
			return func() tea.Msg {
				r, err := f.Fork(c, u, s.repo, "")
				if err != nil {
					return toastErr(err)()
				}
				return batchMsg{toast("forked to " + r.FullName()), push(NewRepoPage(s.ctx, r))}
			}
		})), true
	}
	return nil, false
}

// ---------------------------------------------------------------- code

func (s *repoState) codeTab() *Tab {
	st := s.ctx.St
	t := &Tab{Name: "Code", List: NewList(st, Column{Title: "NAME", Flex: 2}, Column{Title: "SIZE", Width: 10, Right: true})}
	t.List.NoHeader = true
	t.Header = func() string {
		p := "/" + s.path
		return " " + st.Label.Render(" "+trimRef(s.ref)+" ") + " " + st.Bold.Render(p)
	}
	t.Load = func() []Row {
		ctx := s.ctx.Ctx
		if s.git.IsEmpty(ctx) {
			s.readme = readmeCache{}
			t.List.Empty = "This repository is empty. Push your first commit:\n\n" +
				"    git remote add origin " + s.ctx.F.Cfg.SSHCloneURL(s.repo.FullName()) + "\n" +
				"    git push -u origin main\n\n" +
				"or with jj:\n\n" +
				"    jj git remote add origin " + s.ctx.F.Cfg.SSHCloneURL(s.repo.FullName()) + "\n" +
				"    jj bookmark create main -r @- && jj git push --bookmark main"
			return nil
		}
		entries, err := s.git.Tree(ctx, s.ref, s.path)
		if err != nil {
			t.List.Empty = "Cannot read tree: " + err.Error()
			return nil
		}
		s.loadReadme(entries)
		var rows []Row
		if s.path != "" {
			rows = append(rows, Row{Cols: []string{st.Faint.Render("📁 ..")}, Data: gitx.TreeEntry{Type: "up"}})
		}
		for _, e := range entries {
			switch e.Type {
			case "tree":
				rows = append(rows, Row{Cols: []string{st.Accent.Render("📁 " + e.Name + "/"), ""}, Filter: e.Name, Data: e})
			case "commit":
				rows = append(rows, Row{Cols: []string{st.Faint.Render("⎘ " + e.Name + " (submodule)"), ""}, Filter: e.Name, Data: e})
			default:
				rows = append(rows, Row{Cols: []string{"  " + e.Name, st.Faint.Render(humanSize(e.Size))}, Filter: e.Name, Data: e})
			}
		}
		return rows
	}
	t.Below = s.readmeView
	t.BelowMore = "… press v to read the whole README"
	t.Keys = []Key{{"enter", "open"}, {"b", "branch/tag"}, {"-", "parent dir"}, {"v", "README"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "v":
			if s.readme.name == "" {
				return toast("no README in this directory"), true
			}
			return push(NewTextPage(s.ctx, s.readme.name, s.readmeView(s.page.w))), true
		case "b":
			return push(s.refPicker()), true
		case "-":
			if s.path != "" {
				s.path = path.Dir(s.path)
				if s.path == "." {
					s.path = ""
				}
				t.List.SetCursor(0)
				s.page.reload()
				return nil, true
			}
		case "enter":
			if row == nil {
				return nil, true
			}
			e := row.Data.(gitx.TreeEntry)
			switch e.Type {
			case "up":
				return t.Handle("-", nil)
			case "tree":
				s.path = path.Join(s.path, e.Name)
				t.List.SetCursor(0)
				s.page.reload()
				return nil, true
			case "blob":
				return push(s.filePage(path.Join(s.path, e.Name))), true
			}
		}
		return nil, false
	}
	return t
}

func (s *repoState) filePage(p string) Page { return s.filePageAt(p, 0) }

// filePageAt shows a file with line mark highlighted and in view.
func (s *repoState) filePageAt(p string, mark int) Page {
	data, trunc, err := s.git.Blob(s.ctx.Ctx, s.ref, p, 2<<20)
	var content string
	lfsNote := ""
	size := int64(len(data))
	if ptr, ok := gitx.ParseLFSPointer(data); ok && err == nil {
		// show the real file instead of its pointer, if the server has it
		st := s.ctx.St
		lfsNote = st.Accent.Render("◆ Git LFS") + st.Faint.Render(fmt.Sprintf(" · %s · sha256:%s", humanSize(ptr.Size), ptr.OID[:12]))
		if obj, otrunc, oerr := s.ctx.F.ReadLFSObject(s.ctx.Ctx, s.repo, ptr.OID, 2<<20); oerr == nil {
			data, trunc, size = obj, otrunc, ptr.Size
		} else {
			data, trunc = nil, false
			content = lfsNote + "\n\n" + st.Faint.Render("The file's content has not been uploaded to this server (git lfs push).")
		}
	}
	switch {
	case content != "":
	case err != nil:
		content = s.ctx.St.Error.Render(err.Error())
	case gitx.IsBinary(data):
		content = s.ctx.St.Faint.Render(fmt.Sprintf("Binary file (%s) not shown.", humanSize(size)))
	default:
		content = s.ctx.St.Highlight(p, expandTabs(strings.TrimRight(string(data), "\n")))
		if trunc {
			content += "\n" + s.ctx.St.Faint.Render("… file truncated at 2 MB")
		}
	}
	if lfsNote != "" && !strings.HasPrefix(content, lfsNote) {
		content = lfsNote + "\n\n" + content
	}
	tp := NewTextPage(s.ctx, path.Base(p), content)
	tp.gutter = err == nil && !gitx.IsBinary(data) && lfsNote == ""
	tp.mark = mark
	tp.keys = []Key{{"L", "history"}}
	tp.handle = func(k string) (tea.Cmd, bool) {
		if k == "L" {
			return push(s.historyPage(p)), true
		}
		return nil, false
	}
	return tp
}

func (s *repoState) historyPage(p string) Page {
	t := s.commitList(p)
	return NewTabPage(s.ctx, "history: "+path.Base(p), []*Tab{t})
}

func (s *repoState) refPicker() Page {
	st := s.ctx.St
	t := &Tab{Name: "Switch branch or tag", List: NewList(st, Column{Title: "REF", Flex: 1}, Column{Title: "TYPE", Width: 6}, Column{Title: "UPDATED", Width: 10, Right: true})}
	t.Load = func() []Row {
		var rows []Row
		bs, _ := s.git.Branches(s.ctx.Ctx)
		for _, b := range bs {
			rows = append(rows, Row{Cols: []string{b.Name, "branch", st.Faint.Render(ago(b.When))}, Filter: b.Name, Data: b.Full})
		}
		ts, _ := s.git.Tags(s.ctx.Ctx)
		for _, tg := range ts {
			rows = append(rows, Row{Cols: []string{tg.Name, "tag", st.Faint.Render(ago(tg.When))}, Filter: tg.Name, Data: tg.Full})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "select"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k == "enter" && row != nil {
			s.ref = row.Data.(string)
			s.path = ""
			return pop, true
		}
		return nil, false
	}
	return NewTabPage(s.ctx, "refs", []*Tab{t})
}

// ---------------------------------------------------------------- commits

func (s *repoState) commitList(p string) *Tab {
	st := s.ctx.St
	t := &Tab{Name: "Commits", List: NewList(st, Column{Title: "SHA", Width: 8}, Column{Title: "MESSAGE", Flex: 3}, Column{Title: "AUTHOR", Width: 16}, Column{Title: "WHEN", Width: 10, Right: true})}
	t.Header = func() string { return " " + st.Label.Render(" "+trimRef(s.ref)+" ") }
	t.Load = func() []Row {
		cs, err := s.git.Log(s.ctx.Ctx, s.ref, 0, 500, p)
		if err != nil {
			t.List.Empty = "No commits."
		}
		var rows []Row
		for _, c := range cs {
			c := c
			rows = append(rows, Row{Cols: []string{st.Accent.Render(c.Short()), changeLabel(st, c.ChangeID) + c.Subject, st.Subtle.Render(c.Author), st.Faint.Render(ago(c.When))},
				Filter: c.SHA + " " + c.ChangeID + " " + c.Subject + " " + c.Author, Data: &c})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "view diff"}, {"b", "branch/tag"}, {"R", "revert"}, {"P", "cherry-pick"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "b":
			return push(s.refPicker()), true
		case "R":
			if row == nil {
				return nil, true
			}
			if s.ctx.Access(s.repo) < store.AccessWrite {
				return toastErr(forgeForbidden), true
			}
			cm := row.Data.(*gitx.Commit)
			branch := s.repo.DefaultBranch
			if b, ok := strings.CutPrefix(s.ref, "refs/heads/"); ok {
				branch = b
			}
			return push(revertForm(s.ctx, s.repo, "Revert "+cm.Short()+" "+cm.Subject, branch, func(o forge.RevertOptions) (*forge.RevertResult, error) {
				return s.ctx.F.RevertCommit(s.ctx.Ctx, s.ctx.User, s.repo, cm.SHA, o)
			})), true
		case "P":
			if row == nil {
				return nil, true
			}
			if s.ctx.Access(s.repo) < store.AccessWrite {
				return toastErr(forgeForbidden), true
			}
			cm := row.Data.(*gitx.Commit)
			// viewing another branch: offer the default branch as the target
			target := ""
			if trimRef(s.ref) != s.repo.DefaultBranch {
				target = s.repo.DefaultBranch
			}
			return push(pickForm(s.ctx, s.repo, "Cherry-pick "+cm.Short()+" "+cm.Subject, target, func(o forge.RevertOptions) (*forge.RevertResult, error) {
				return s.ctx.F.CherryPick(s.ctx.Ctx, s.ctx.User, s.repo, cm.SHA, o)
			})), true
		case "enter":
			if row != nil {
				return push(s.commitPage(row.Data.(*gitx.Commit))), true
			}
		}
		return nil, false
	}
	return t
}

func (s *repoState) commitsTab() *Tab { return s.commitList("") }

func (s *repoState) commitPage(c *gitx.Commit) Page {
	st := s.ctx.St
	diffs, err := s.git.Diff(s.ctx.Ctx, "", c.SHA)
	var b strings.Builder
	b.WriteString(st.Title.Render(c.Subject) + "\n")
	if c.Body != "" {
		b.WriteString("\n" + c.Body + "\n")
	}
	b.WriteString("\n" + st.Subtle.Render(fmt.Sprintf("%s <%s> · %s · %s", c.Author, c.AuthorEmail, c.When.Format("2006-01-02 15:04"), c.SHA)) + "\n")
	if c.ChangeID != "" {
		b.WriteString(st.Faint.Render("jj change ") + st.Label.Render(c.ChangeID) + "\n")
	}
	if len(c.Parents) > 0 {
		b.WriteString(st.Faint.Render("parents "+strings.Join(c.Parents, " ")) + "\n")
	}
	if runs, _ := s.ctx.F.Store.RunsForSHA(s.ctx.Ctx, s.repo.ID, c.SHA); len(runs) > 0 {
		b.WriteString("\n")
		for _, r := range runs {
			b.WriteString(fmt.Sprintf("%s %s #%d\n", st.StatusIcon(r.Status), r.Pipeline, r.Number))
		}
	}
	if err != nil {
		b.WriteString(st.Error.Render(err.Error()))
	} else {
		b.WriteString("\n" + renderDiffs(st, diffs))
	}
	return NewTextPage(s.ctx, c.Short(), b.String())
}

func renderDiffs(st *Styles, diffs []gitx.FileDiff) string {
	var b strings.Builder
	add, del := 0, 0
	for _, d := range diffs {
		add += d.Additions
		del += d.Deletions
	}
	b.WriteString(st.Subtle.Render(fmt.Sprintf("%d file(s) changed, ", len(diffs))) + st.Add.Render(fmt.Sprintf("+%d", add)) + " " + st.Del.Render(fmt.Sprintf("-%d", del)) + "\n")
	for _, d := range diffs {
		b.WriteString(fmt.Sprintf("  %s %s %s\n", st.Faint.Render(d.Status), d.NewPath, st.Add.Render(fmt.Sprintf("+%d", d.Additions))+st.Del.Render(fmt.Sprintf(" -%d", d.Deletions))))
	}
	for _, d := range diffs {
		name := d.NewPath
		if d.Status == "R" {
			name = d.OldPath + " → " + d.NewPath
		}
		b.WriteString("\n" + st.FileHdr.Render("━━ "+d.Status+" "+name) + "\n")
		if d.Binary {
			b.WriteString(st.Faint.Render("binary file") + "\n")
			continue
		}
		b.WriteString(st.HighlightDiff(d.NewPath, d.Patch))
	}
	return b.String()
}

// ---------------------------------------------------------------- branches & tags

func (s *repoState) branchesTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Branches", List: NewList(st, Column{Title: "", Width: 1}, Column{Title: "BRANCH", Flex: 1}, Column{Title: "SHA", Width: 8}, Column{Title: "LAST COMMIT", Flex: 2}, Column{Title: "UPDATED", Width: 10, Right: true})}
	t.List.Empty = "No branches yet. Push one."
	t.Load = func() []Row {
		var rows []Row
		bs, _ := s.git.Branches(c)
		rules, _ := f.Store.Protections(c, s.repo.ID)
		for _, b := range bs {
			mark := " "
			if b.Name == s.repo.DefaultBranch {
				mark = st.Label.Render("★")
			}
			name := st.Bold.Render(b.Name)
			if _, ok := protect.Effective(rules, b.Name); ok {
				name += st.StatusRun.Render(" 🔒")
			}
			icon := ""
			if runs, _ := f.Store.RunsForSHA(c, s.repo.ID, b.SHA); len(runs) > 0 {
				icon = st.StatusIcon(runs[0].Status) + " "
			}
			rows = append(rows, Row{Cols: []string{mark, name, st.Accent.Render(shortSHA(b.SHA)), icon + b.Subject, st.Faint.Render(ago(b.When))}, Filter: b.Name, Data: b})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "browse"}, {"n", "new branch"}, {"p", "open PR"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k == "n" {
			from := s.repo.DefaultBranch
			if row != nil {
				from = row.Data.(gitx.Ref).Name
			}
			return push(NewForm(s.ctx, "New branch", []*Field{
				{Label: "Name", Placeholder: "feature/login"},
				{Label: "From", Value: from, Hint: "branch, tag or commit"},
			}, func(v map[string]string) (tea.Cmd, error) {
				if err := f.CreateBranch(c, s.ctx.User, s.repo, v["Name"], v["From"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("created branch "+v["Name"]), refreshCmd), nil
			})), true
		}
		if row == nil {
			return nil, false
		}
		ref := row.Data.(gitx.Ref)
		if k == "p" {
			return push(s.newPRForm(ref.Name)), true
		}
		return s.refKey(k, ref)
	}
	return t
}

func (s *repoState) tagsTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Tags", List: NewList(st, Column{Title: "TAG", Flex: 1}, Column{Title: "SHA", Width: 8}, Column{Title: "COMMIT", Flex: 2}, Column{Title: "RELEASE", Flex: 1}, Column{Title: "DATE", Width: 10, Right: true})}
	t.List.Empty = "No tags. Press n to create one, or push one (git push origin v1.0)."
	t.Load = func() []Row {
		released := map[string]string{}
		if rs, err := f.Store.Releases(c, s.repo.ID); err == nil {
			for _, r := range rs {
				released[r.Tag] = r.Title
				if r.Title == "" {
					released[r.Tag] = r.Tag
				}
			}
		}
		var rows []Row
		ts, _ := s.git.Tags(c)
		for _, tg := range ts {
			rel := ""
			if title, ok := released[tg.Name]; ok {
				rel = st.StatusOK.Render("● ") + title
			}
			rows = append(rows, Row{Cols: []string{st.Label.Render(tg.Name), st.Accent.Render(shortSHA(tg.SHA)), tg.Subject, rel, st.Faint.Render(ago(tg.When))}, Filter: tg.Name, Data: tg})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "browse"}, {"n", "new tag"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if k == "n" {
			return push(NewForm(s.ctx, "New tag", []*Field{
				{Label: "Name", Placeholder: "v1.2.0"},
				{Label: "Target", Value: s.repo.DefaultBranch, Hint: "branch, tag or commit"},
				{Label: "Message", Hint: "optional; makes it an annotated tag"},
			}, func(v map[string]string) (tea.Cmd, error) {
				if err := f.CreateTag(c, s.ctx.User, s.repo, v["Name"], v["Target"], v["Message"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("created tag "+v["Name"]), refreshCmd), nil
			})), true
		}
		if row == nil {
			return nil, false
		}
		return s.refKey(k, row.Data.(gitx.Ref))
	}
	return t
}

// refKey handles the keys the Branches and Tags tabs share.
func (s *repoState) refKey(k string, ref gitx.Ref) (tea.Cmd, bool) {
	f, c := s.ctx.F, s.ctx.Ctx
	switch k {
	case "enter":
		s.ref, s.path = ref.Full, ""
		s.page.t.Active = 0
		s.page.SetSize(s.page.w, s.page.h)
		s.page.reload()
		return nil, true
	case "d":
		if ref.Name == s.repo.DefaultBranch && !ref.IsTag {
			return toastErr(fmt.Errorf("cannot delete the default branch")), true
		}
		if s.ctx.Access(s.repo) < store.AccessWrite {
			return toastErr(forgeForbidden), true
		}
		if s.repo.Mirror {
			return toastErr(f.ReadOnlyError(c, s.repo)), true
		}
		return push(NewConfirm(s.ctx, "Delete "+ref.Full+"?", func() tea.Cmd {
			return do(func() error {
				if !ref.IsTag {
					return f.DeleteBranch(c, s.ctx.User, s.repo, ref.Name) // checks protection
				}
				before, _ := s.git.RefMap(c)
				if err := s.git.DeleteRef(c, ref.Full); err != nil {
					return err
				}
				after, _ := s.git.RefMap(c)
				f.PostReceive(c, s.ctx.User, s.repo, diffRefs(before, after))
				return nil
			}, "deleted "+ref.Name, refreshCmd)
		})), true
	}
	return nil, false
}

// ---------------------------------------------------------------- issues & PRs

func (s *repoState) boardTab() *Tab {
	b := newBoardPane(s.ctx, s.repo)
	return &Tab{Name: "Board", Custom: b, Header: b.Header}
}

func (s *repoState) issuesTab(prs bool) *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	name := "Issues"
	state := &s.issueState
	if prs {
		name, state = "Pull requests", &s.prState
	}
	cols := []Column{{Title: "", Width: 1}, {Title: "#", Width: 5, Right: true}, {Title: "TITLE", Flex: 3}}
	if prs {
		cols = append(cols, Column{Title: "BRANCH", Flex: 1}, Column{Title: "CI", Width: 2})
	}
	cols = append(cols, Column{Title: "AUTHOR", Width: 12}, Column{Title: "💬", Width: 3, Right: true}, Column{Title: "UPDATED", Width: 10, Right: true})
	t := &Tab{Name: name, List: NewList(st, cols...)}
	t.Header = func() string {
		var parts []string
		for _, sname := range []string{"open", "closed", "all"} {
			if sname == *state {
				parts = append(parts, st.Label.Render("["+sname+"]"))
			} else {
				parts = append(parts, st.Faint.Render(sname))
			}
		}
		return " " + strings.Join(parts, " ") + st.Faint.Render("   (s to switch)")
	}
	t.Load = func() []Row {
		fs := *state
		if fs == "all" {
			fs = ""
		}
		is, _ := f.Store.ListIssues(c, s.repo.ID, store.IssueFilter{PRs: prs, State: fs})
		var rows []Row
		for _, i := range is {
			cols := []string{st.StatusIcon(i.State), st.Faint.Render(fmt.Sprint(i.Number)), st.Bold.Render(i.Title) + labelsView(st, i.Labels)}
			if prs {
				br := i.PR.HeadBranch + " → " + i.PR.BaseBranch
				if i.PR.HeadRepoID != s.repo.ID {
					br = i.PR.HeadRepo + ":" + br
				}
				ci := ""
				if runs, _ := f.Store.RunsForSHA(c, s.repo.ID, i.PR.HeadSHA); len(runs) > 0 {
					ci = st.StatusIcon(runs[0].Status)
				}
				cols = append(cols, st.Accent.Render(br), ci)
			}
			cols = append(cols, st.Subtle.Render(i.Author), st.Faint.Render(fmt.Sprint(i.Comments)), st.Faint.Render(ago(i.UpdatedAt)))
			rows = append(rows, Row{Cols: cols, Filter: fmt.Sprintf("#%d %s %s %s", i.Number, i.Title, i.Author, strings.Join(i.Labels, " ")), Data: i})
		}
		if prs {
			t.List.Empty = "No pull requests. Press n to open one, or p on a branch in Branches."
		} else {
			t.List.Empty = "No issues. Press n to open one."
		}
		return rows
	}
	t.Keys = []Key{{"enter", "open"}, {"n", "new"}, {"s", "state filter"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "s":
			switch *state {
			case "open":
				*state = "closed"
			case "closed":
				*state = "all"
			default:
				*state = "open"
			}
			s.page.reload()
			return nil, true
		case "n":
			if s.ctx.User == nil {
				return toastErr(fmt.Errorf("sign in to participate")), true
			}
			if prs {
				return push(s.newPRForm("")), true
			}
			return push(NewForm(s.ctx, "New issue in "+s.repo.FullName(), []*Field{{Label: "Title"}, {Label: "Description", Multiline: true, Placeholder: "Markdown welcome"}},
				func(v map[string]string) (tea.Cmd, error) {
					i, err := f.CreateIssue(c, s.ctx.User, s.repo, v["Title"], v["Description"])
					if err != nil {
						return nil, err
					}
					return tea.Sequence(pop, toast(fmt.Sprintf("opened #%d", i.Number)), push(NewIssuePage(s.ctx, s.repo, i))), nil
				})), true
		case "enter":
			if row != nil {
				return push(NewIssuePage(s.ctx, s.repo, row.Data.(*store.Issue))), true
			}
		}
		return nil, false
	}
	return t
}

func labelsView(st *Styles, labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	var parts []string
	for _, l := range labels {
		parts = append(parts, st.Label.Render("["+l+"]"))
	}
	return " " + strings.Join(parts, " ")
}

func (s *repoState) newPRForm(head string) Page {
	f, c := s.ctx.F, s.ctx.Ctx
	if s.ctx.User == nil {
		return NewConfirm(s.ctx, "Sign in to open pull requests.", func() tea.Cmd { return nil })
	}
	var branches []string
	bs, _ := s.git.Branches(c)
	for _, b := range bs {
		if b.Name == head {
			branches = append([]string{b.Name}, branches...)
		} else {
			branches = append(branches, b.Name)
		}
	}
	if len(branches) == 0 {
		return NewConfirm(s.ctx, "Push a branch first.", func() tea.Cmd { return nil })
	}
	targets := []string{s.repo.FullName()}
	if s.repo.ForkOfName != "" {
		targets = []string{s.repo.ForkOfName, s.repo.FullName()}
	}
	base := s.repo.DefaultBranch
	return NewForm(s.ctx, "Open pull request", []*Field{
		{Label: "From branch", Options: branches, Hint: "in " + s.repo.FullName()},
		{Label: "Into repository", Options: targets},
		{Label: "Into branch", Value: base},
		{Label: "Title", Placeholder: "defaults to the head commit subject"},
		{Label: "Description", Multiline: true},
	}, func(v map[string]string) (tea.Cmd, error) {
		baseRepo, _, err := f.ResolveRepo(c, s.ctx.User, v["Into repository"], store.AccessRead)
		if err != nil {
			return nil, err
		}
		i, err := f.OpenPR(c, s.ctx.User, baseRepo, v["Into branch"], s.repo, v["From branch"], v["Title"], v["Description"])
		if err != nil {
			return nil, err
		}
		return tea.Sequence(pop, toast(fmt.Sprintf("opened PR #%d", i.Number)), push(NewIssuePage(s.ctx, baseRepo, i))), nil
	})
}

// ---------------------------------------------------------------- pipelines

func (s *repoState) pipelinesTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Pipelines", List: NewList(st, Column{Title: "", Width: 1}, Column{Title: "RUN", Width: 6}, Column{Title: "PIPELINE", Flex: 1}, Column{Title: "JOBS", Width: 12},
		Column{Title: "TRIGGER", Width: 18}, Column{Title: "REF", Flex: 1}, Column{Title: "SHA", Width: 8, HideBelow: 110}, Column{Title: "DURATION", Width: 8, Right: true},
		Column{Title: "CREATED", Width: 10, Right: true, HideBelow: 96})}
	t.List.Empty = "No pipeline runs. Add a .forge.yml (or .forge/pipelines/*.yml) and push."
	t.Load = func() []Row {
		runs, _ := f.Store.Runs(c, s.repo.ID, 300)
		var rows []Row
		for i, r := range runs {
			trig := st.Trigger(r.Trigger)
			if r.Actor != "" {
				trig += st.Faint.Render(" · " + r.Actor)
			}
			ref := trimRef(r.Ref)
			if r.PRNumber > 0 {
				ref = fmt.Sprintf("PR #%d", r.PRNumber)
			}
			strip := ""
			if i < 100 { // one query per run: only for the recent ones
				if jobs, err := f.Store.Jobs(c, r.ID); err == nil {
					strip = st.JobStrip(jobs)
				}
			}
			rows = append(rows, Row{Cols: []string{st.StatusIcon(r.Status), st.statusStyle(r.Status).Render(fmt.Sprintf("#%d", r.Number)), st.Bold.Render(r.Pipeline), strip, trig,
				st.Accent.Render(ref), st.Faint.Render(shortSHA(r.SHA)), st.Duration(dur(r.Duration()), r.Status), st.Faint.Render(ago(r.CreatedAt))},
				Filter: fmt.Sprintf("#%d %s %s %s %s %s", r.Number, r.Pipeline, r.Trigger, r.Ref, r.Status, r.SHA), Data: r})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "inspect"}, {"t", "trigger"}, {"A", "approve"}, {"c", "cancel"}, {"R", "rerun"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "t":
			return push(s.triggerForm()), true
		case "enter":
			if row != nil {
				return push(NewRunPage(s.ctx, s.repo, row.Data.(*store.Run))), true
			}
		case "c":
			if row != nil {
				r := row.Data.(*store.Run)
				return do(func() error { return f.CancelRun(c, s.ctx.User, s.repo, r) }, fmt.Sprintf("cancelling #%d", r.Number), refreshCmd), true
			}
		case "R":
			if row != nil {
				r := row.Data.(*store.Run)
				return do(func() error { return f.RerunRun(c, s.ctx.User, s.repo, r, false) }, fmt.Sprintf("re-running #%d", r.Number), refreshCmd), true
			}
		case "A":
			if row != nil {
				return approveRun(s.ctx, s.repo, row.Data.(*store.Run)), true
			}
		}
		return nil, false
	}
	return t
}

func (s *repoState) triggerForm() Page {
	f, c := s.ctx.F, s.ctx.Ctx
	names := []string{"(all manual pipelines)"}
	if sha, err := s.git.ResolveRef(c, s.ref); err == nil {
		pipes, _ := ci.LoadPipelines(c, s.git, sha)
		for file, p := range pipes {
			n := p.Name
			if n == "" {
				n = file
			}
			names = append(names, n)
		}
	}
	return NewForm(s.ctx, "Run pipeline", []*Field{
		{Label: "Ref", Value: trimRef(s.ref), Hint: "branch, tag or SHA"},
		{Label: "Pipeline", Options: names},
	}, func(v map[string]string) (tea.Cmd, error) {
		p := v["Pipeline"]
		if strings.HasPrefix(p, "(") {
			p = ""
		}
		runs, err := f.TriggerManual(c, s.ctx.User, s.repo, v["Ref"], p)
		if err != nil {
			return nil, err
		}
		if len(runs) == 1 {
			return tea.Sequence(pop, toast(fmt.Sprintf("started #%d", runs[0].Number)), push(NewRunPage(s.ctx, s.repo, runs[0]))), nil
		}
		return tea.Sequence(pop, toast(fmt.Sprintf("started %d run(s)", len(runs)))), nil
	})
}

// ---------------------------------------------------------------- releases

func (s *repoState) releasesTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Releases", List: NewList(st, Column{Title: "TAG", Width: 16}, Column{Title: "TITLE", Flex: 2}, Column{Title: "AUTHOR", Width: 12}, Column{Title: "PUBLISHED", Width: 10, Right: true})}
	t.List.Empty = "No releases. Press n to publish one."
	t.Load = func() []Row {
		rs, _ := f.Store.Releases(c, s.repo.ID)
		var rows []Row
		for _, r := range rs {
			rows = append(rows, Row{Cols: []string{st.Label.Render(r.Tag), st.Bold.Render(r.Title), st.Subtle.Render(r.Author), st.Faint.Render(ago(r.CreatedAt))}, Data: r})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "notes"}, {"n", "new release"}, {"d", "delete"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		switch k {
		case "n":
			return push(NewForm(s.ctx, "New release", []*Field{
				{Label: "Tag", Placeholder: "v1.0.0", Hint: "created if it doesn't exist"},
				{Label: "Target", Value: s.repo.DefaultBranch, Hint: "for new tags"},
				{Label: "Title"},
				{Label: "Notes", Multiline: true},
			}, func(v map[string]string) (tea.Cmd, error) {
				if v["Tag"] == "" {
					return nil, fmt.Errorf("tag is required")
				}
				if _, err := f.CreateRelease(c, s.ctx.User, s.repo, v["Tag"], v["Target"], v["Title"], v["Notes"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("release published")), nil
			})), true
		case "enter":
			if row != nil {
				r := row.Data.(*store.Release)
				body := st.Title.Render(r.Title) + "  " + st.Label.Render(r.Tag) + "\n" + st.Subtle.Render("by "+r.Author+" · "+r.CreatedAt.Format("2006-01-02")) + "\n\n" + r.Notes
				return push(NewTextPage(s.ctx, r.Tag, body)), true
			}
		case "d":
			if row != nil {
				r := row.Data.(*store.Release)
				if s.ctx.Access(s.repo) < store.AccessWrite {
					return toastErr(forgeForbidden), true
				}
				return push(NewConfirm(s.ctx, "Delete release "+r.Tag+"? (the tag is kept)", func() tea.Cmd {
					return do(func() error { return f.Store.DeleteRelease(c, s.repo.ID, r.Tag) }, "release deleted", refreshCmd)
				})), true
			}
		}
		return nil, false
	}
	return t
}

func (s *repoState) activityTab() *Tab {
	st := s.ctx.St
	t := &Tab{Name: "Activity", List: NewList(st, Column{Title: "EVENT", Flex: 1}, Column{Title: "WHEN", Width: 10, Right: true})}
	t.Load = func() []Row {
		evs, _ := s.ctx.F.Store.RepoEvents(s.ctx.Ctx, s.repo.ID, 300)
		var rows []Row
		for _, e := range evs {
			rows = append(rows, Row{Cols: []string{e.Summary, st.Faint.Render(ago(e.CreatedAt))}})
		}
		return rows
	}
	return t
}

// mirrorStatus is the pull mirror's state for the repository header.
func mirrorStatus(st *Styles, ctx *Ctx, r *store.Repo) string {
	m, err := ctx.F.Store.PullMirror(ctx.Ctx, r.ID)
	if err != nil {
		return ""
	}
	out := st.Accent.Render("⇣ mirror of "+m.URL) + st.Faint.Render(" · read-only")
	switch {
	case m.LastError != "":
		out += "  " + st.StatusFail.Render("✗ sync failed "+ago(m.LastTry)+": "+m.LastError)
	case m.LastSync.IsZero():
		out += "  " + st.StatusRun.Render("syncing…")
	default:
		out += st.Faint.Render(" · synced " + ago(m.LastSync))
	}
	return out
}

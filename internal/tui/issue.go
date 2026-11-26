package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// viewPane is a scrollable Pane whose content is regenerated on Load.
type viewPane struct {
	vp     viewport.Model
	render func(w int) string
	keys   []Key
	handle func(k string) (tea.Cmd, bool)
	w      int
	follow bool
}

func newViewPane(render func(int) string) *viewPane {
	return &viewPane{vp: viewport.New(80, 20), render: render}
}

func (v *viewPane) Load() {
	atBottom := v.vp.AtBottom()
	v.vp.SetContent(v.render(v.w))
	if v.follow && atBottom {
		v.vp.GotoBottom()
	}
}
func (v *viewPane) SetSize(w, h int) { v.w = w; v.vp.Width, v.vp.Height = w, h; v.Load() }
func (v *viewPane) Keys() []Key      { return append(append([]Key{}, v.keys...), Key{"↑↓", "scroll"}) }
func (v *viewPane) Capturing() bool  { return false }
func (v *viewPane) View() string     { return v.vp.View() }
func (v *viewPane) Update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case refreshMsg:
		v.Load()
		return nil, true
	case tea.KeyMsg:
		if v.handle != nil {
			if cmd, ok := v.handle(msg.String()); ok {
				return cmd, true
			}
		}
		switch msg.String() {
		case "g":
			v.vp.GotoTop()
			return nil, true
		case "G":
			v.vp.GotoBottom()
			return nil, true
		case "up", "down", "k", "j", "pgup", "pgdown", "ctrl+u", "ctrl+d", " ":
			var cmd tea.Cmd
			v.vp, cmd = v.vp.Update(msg)
			return cmd, true
		}
	}
	return nil, false
}

type issueState struct {
	ctx   *Ctx
	repo  *store.Repo
	issue *store.Issue
	prs   *forge.PRStatus
}

func NewIssuePage(ctx *Ctx, r *store.Repo, i *store.Issue) Page {
	s := &issueState{ctx: ctx, repo: r, issue: i}
	conv := newViewPane(s.renderConversation)
	conv.keys = []Key{{"c", "comment"}, {"e", "edit"}, {"x", "close/reopen"}, {"L", "labels"}}
	if i.IsPR {
		conv.keys = append(conv.keys, Key{"A", "approve"}, Key{"C", "request changes"}, Key{"m", "merge"}, Key{"R", "revert"}, Key{"P", "cherry-pick"})
	}
	conv.handle = s.handle
	tabs := []*Tab{{Name: "Conversation", Custom: conv}}
	if i.IsPR {
		commits := newViewPane(s.renderCommits)
		commits.handle = s.handle
		files := newViewPane(s.renderFiles)
		files.keys = []Key{{"v", "review comment"}}
		files.handle = s.handle
		checks := &Tab{Name: "Checks", List: NewList(ctx.St, Column{Title: "", Width: 1}, Column{Title: "PIPELINE", Flex: 1}, Column{Title: "RUN", Width: 6}, Column{Title: "STATUS", Width: 12}, Column{Title: "DURATION", Width: 9, Right: true})}
		checks.List.Empty = "No pipeline runs for the head commit."
		checks.Load = func() []Row {
			s.reload()
			var rows []Row
			if s.prs != nil {
				for _, run := range s.prs.Checks {
					rows = append(rows, Row{Cols: []string{ctx.St.StatusIcon(run.Status), ctx.St.Bold.Render(run.Pipeline), fmt.Sprintf("#%d", run.Number), ctx.St.StatusText(run.Status), dur(run.Duration())}, Data: run})
				}
			}
			return rows
		}
		checks.Keys = []Key{{"enter", "inspect run"}}
		checks.Handle = func(k string, row *Row) (tea.Cmd, bool) {
			if k == "enter" && row != nil {
				return push(NewRunPage(ctx, r, row.Data.(*store.Run))), true
			}
			return nil, false
		}
		tabs = append(tabs, &Tab{Name: "Commits", Custom: commits}, &Tab{Name: "Files", Custom: files}, checks, s.revisionsTab())
	}
	title := fmt.Sprintf("#%d", i.Number)
	p := NewTabPage(ctx, title, tabs)
	p.top = func() string {
		st := ctx.St
		is := s.issue
		line := " " + st.StatusText(is.State) + "  " + st.Title.Render(is.Title) + labelsView(st, is.Labels)
		sub := " " + st.Subtle.Render(fmt.Sprintf("#%d opened by %s %s", is.Number, is.Author, ago(is.CreatedAt)))
		if is.IsPR {
			head := is.PR.HeadBranch
			if is.PR.HeadRepoID != r.ID {
				head = is.PR.HeadRepo + ":" + head
			}
			sub += st.Subtle.Render(" · ") + st.Accent.Render(head) + st.Subtle.Render(" → ") + st.Accent.Render(is.PR.BaseBranch)
		}
		return line + "\n" + sub
	}
	return p
}

func (s *issueState) reload() {
	if i, err := s.ctx.F.Store.IssueByNumber(s.ctx.Ctx, s.repo.ID, s.issue.Number); err == nil {
		s.issue = i
	}
	if s.issue.IsPR {
		s.prs, _ = s.ctx.F.PRStatus(s.ctx.Ctx, s.repo, s.issue)
	}
}

func (s *issueState) renderConversation(w int) string {
	s.reload()
	st := s.ctx.St
	i := s.issue
	cw := w - 4
	if cw > 110 {
		cw = 110
	}
	var b strings.Builder
	body := st.Markdown(i.Body, cw-4)
	if strings.TrimSpace(i.Body) == "" {
		body = st.Faint.Render("No description provided.")
	}
	b.WriteString(commentBox(st, i.Author, i.CreatedAt.Format("2006-01-02 15:04"), "", body, cw) + "\n")
	if i.IsPR && s.prs != nil {
		b.WriteString(s.mergeBox(cw) + "\n")
	}
	comments, _ := s.ctx.F.Store.Comments(s.ctx.Ctx, i.ID)
	for _, c := range comments {
		badge := ""
		switch c.Review {
		case "approve":
			badge = st.StatusOK.Render("✓ approved")
		case "changes":
			badge = st.StatusFail.Render("✗ requested changes")
		case "comment":
			badge = st.Accent.Render("review comment")
		}
		text := st.Markdown(c.Body, cw-4)
		if c.Path != "" {
			loc := c.Path
			if c.Line > 0 {
				loc += fmt.Sprintf(":%d", c.Line)
			}
			text = wrap(st.Accent.Render(loc)+s.codeContext(c.Path, c.Line), cw-4) + "\n" + text
		}
		b.WriteString(commentBox(st, c.Author, c.CreatedAt.Format("2006-01-02 15:04"), badge, text, cw) + "\n")
	}
	if len(comments) == 0 {
		b.WriteString(st.Faint.Render("  No comments yet — press c to add one.") + "\n")
	}
	return b.String()
}

// codeContext shows the referenced line from the PR head for review comments.
func (s *issueState) codeContext(p string, line int) string {
	if line <= 0 || !s.issue.IsPR {
		return ""
	}
	data, _, err := s.ctx.F.Git(s.repo).Blob(s.ctx.Ctx, fmt.Sprintf("refs/pull/%d/head", s.issue.Number), p, 1<<20)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return ""
	}
	return "\n" + s.ctx.St.Faint.Render(fmt.Sprintf("%5d │ ", line)) + expandTabs(lines[line-1])
}

func commentBox(st *Styles, author, when, badge, body string, w int) string {
	head := st.Bold.Render(author) + " " + st.Faint.Render(when)
	if badge != "" {
		head += "  " + badge
	}
	return st.Box.Width(w).Render(head + "\n" + body)
}

func (s *issueState) mergeBox(w int) string {
	st, p, i := s.ctx.St, s.prs, s.issue
	var b strings.Builder
	switch i.State {
	case "merged":
		b.WriteString(st.Label.Render("◆ Merged") + st.Subtle.Render(fmt.Sprintf(" by %s as %s", i.PR.MergedBy, shortSHA(i.PR.MergeSHA))))
	case "closed":
		b.WriteString(st.StatusFail.Render("● Closed without merging"))
	default:
		if p.Mergeable {
			b.WriteString(st.StatusOK.Render("✓ No conflicts with " + i.PR.BaseBranch))
		} else {
			b.WriteString(st.StatusFail.Render("✗ Conflicts with " + i.PR.BaseBranch))
			for _, c := range p.Conflicts {
				b.WriteString("\n    " + st.Faint.Render(c))
			}
		}
	}
	b.WriteString(fmt.Sprintf("\n%s %d commit(s), %d file(s) changed", st.Subtle.Render("•"), len(p.Commits), len(p.Files)))
	if revs, _ := s.ctx.F.Store.PRRevisions(s.ctx.Ctx, i.ID); len(revs) > 1 {
		b.WriteString(st.Subtle.Render(fmt.Sprintf(" · %d revisions", len(revs))) + st.Faint.Render(" (Revisions tab: what changed between pushes)"))
	}
	if below, above := s.ctx.F.PRStack(s.ctx.Ctx, s.repo, i); len(below)+len(above) > 0 {
		var parts []string
		for _, x := range below {
			parts = append(parts, fmt.Sprintf("#%d", x.Number))
		}
		parts = append(parts, st.Bold.Render(fmt.Sprintf("#%d", i.Number)))
		for _, x := range above {
			parts = append(parts, fmt.Sprintf("#%d", x.Number))
		}
		b.WriteString("\n" + st.Subtle.Render("• stack ") + strings.Join(parts, st.Faint.Render(" ← ")))
		if len(below) > 0 {
			b.WriteString(st.Faint.Render(fmt.Sprintf("  (merge #%d first; this PR then targets %s)", below[0].Number, below[0].PR.BaseBranch)))
		}
	}
	if len(p.Checks) == 0 {
		b.WriteString("\n" + st.Subtle.Render("• no checks"))
	}
	for _, r := range p.Checks {
		b.WriteString(fmt.Sprintf("\n%s %s #%d %s", st.StatusIcon(r.Status), r.Pipeline, r.Number, st.Faint.Render(r.Status)))
	}
	if len(p.Approvals) > 0 {
		b.WriteString("\n" + st.StatusOK.Render("✓ approved by "+strings.Join(p.Approvals, ", ")))
	}
	if len(p.Changes) > 0 {
		b.WriteString("\n" + st.StatusFail.Render("✗ changes requested by "+strings.Join(p.Changes, ", ")))
	}
	if i.State == "open" {
		if rule, ok := s.ctx.F.BranchRule(s.ctx.Ctx, s.repo, i.PR.BaseBranch); ok {
			b.WriteString("\n" + st.StatusRun.Render("🔒 "+i.PR.BaseBranch+" is protected") + st.Faint.Render(" · "+rule.Summary()))
			if blockers := s.ctx.F.MergeBlockers(s.ctx.Ctx, s.ctx.User, s.repo, i, p); len(blockers) > 0 {
				for _, bl := range blockers {
					b.WriteString("\n" + st.StatusFail.Render("  ✗ "+bl))
				}
			} else {
				b.WriteString("\n" + st.StatusOK.Render("  ✓ all merge requirements are met"))
			}
		}
		b.WriteString("\n" + st.Faint.Render("press m to merge"))
	}
	return st.BoxFocus.Width(w).Render(b.String())
}

func (s *issueState) renderCommits(w int) string {
	s.reload()
	st := s.ctx.St
	if s.prs == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range s.prs.Commits {
		b.WriteString(fmt.Sprintf(" %s  %s%s  %s  %s\n", st.Accent.Render(c.Short()), changeLabel(st, c.ChangeID), c.Subject, st.Subtle.Render(c.Author), st.Faint.Render(ago(c.When))))
	}
	if len(s.prs.Commits) == 0 {
		b.WriteString(st.Faint.Render(" No commits."))
	}
	return b.String()
}

func (s *issueState) renderFiles(w int) string {
	if s.prs == nil {
		s.reload()
	}
	if s.prs == nil {
		return ""
	}
	return renderDiffs(s.ctx.St, s.prs.Files)
}

func (s *issueState) handle(k string) (tea.Cmd, bool) {
	f, c, u, r, i := s.ctx.F, s.ctx.Ctx, s.ctx.User, s.repo, s.issue
	if u == nil && strings.Contains("cexLACmvRP", k) && len(k) == 1 {
		return toastErr(fmt.Errorf("sign in to participate")), true
	}
	commentForm := func(title, review string) tea.Cmd {
		return push(NewForm(s.ctx, title, []*Field{{Label: "Comment", Multiline: true}}, func(v map[string]string) (tea.Cmd, error) {
			if err := f.Comment(c, u, r, i, &store.Comment{Body: v["Comment"], Review: review}); err != nil {
				return nil, err
			}
			return tea.Sequence(pop, toast("posted")), nil
		}))
	}
	switch k {
	case "c":
		return commentForm(fmt.Sprintf("Comment on #%d", i.Number), ""), true
	case "A":
		if !i.IsPR {
			return nil, false
		}
		return commentForm("Approve pull request", "approve"), true
	case "C":
		if !i.IsPR {
			return nil, false
		}
		return commentForm("Request changes", "changes"), true
	case "v":
		if !i.IsPR || s.prs == nil {
			return nil, false
		}
		var paths []string
		for _, fd := range s.prs.Files {
			paths = append(paths, fd.NewPath)
		}
		if len(paths) == 0 {
			return toastErr(fmt.Errorf("no changed files")), true
		}
		return push(NewForm(s.ctx, "Review comment", []*Field{{Label: "File", Options: paths}, {Label: "Line", Placeholder: "line number in the new file (optional)"}, {Label: "Comment", Multiline: true}},
			func(v map[string]string) (tea.Cmd, error) {
				line, _ := strconv.Atoi(v["Line"])
				if err := f.Comment(c, u, r, i, &store.Comment{Body: v["Comment"], Review: "comment", Path: v["File"], Line: line}); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("review comment posted")), nil
			})), true
	case "e":
		return push(NewForm(s.ctx, fmt.Sprintf("Edit #%d", i.Number), []*Field{{Label: "Title", Value: i.Title}, {Label: "Description", Value: i.Body, Multiline: true}},
			func(v map[string]string) (tea.Cmd, error) {
				cp := *i
				cp.Title, cp.Body = v["Title"], v["Description"]
				if err := f.EditIssue(c, u, r, &cp); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("saved")), nil
			})), true
	case "L":
		return push(NewForm(s.ctx, "Labels", []*Field{{Label: "Labels", Value: strings.Join(i.Labels, ", "), Hint: "comma separated"}},
			func(v map[string]string) (tea.Cmd, error) {
				cp := *i
				cp.Labels = nil
				for _, l := range strings.Split(v["Labels"], ",") {
					if l = strings.TrimSpace(l); l != "" {
						cp.Labels = append(cp.Labels, l)
					}
				}
				if err := f.EditIssue(c, u, r, &cp); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("labels updated")), nil
			})), true
	case "x":
		next := "closed"
		if i.State == "closed" {
			next = "open"
		}
		return do(func() error { return f.SetIssueState(c, u, r, i, next) }, "issue "+next, refreshCmd), true
	case "R":
		if !i.IsPR {
			return nil, false
		}
		if i.PR == nil || i.PR.MergeSHA == "" {
			return toastErr(fmt.Errorf("only merged pull requests can be reverted")), true
		}
		return push(revertForm(s.ctx, r, fmt.Sprintf("Revert #%d %s", i.Number, i.Title), i.PR.BaseBranch, func(o forge.RevertOptions) (*forge.RevertResult, error) {
			return f.RevertPR(c, u, r, i, o)
		})), true
	case "P":
		if !i.IsPR {
			return nil, false
		}
		if i.PR == nil || i.PR.MergeSHA == "" {
			return toastErr(fmt.Errorf("only merged pull requests can be cherry-picked")), true
		}
		return push(pickForm(s.ctx, r, fmt.Sprintf("Cherry-pick #%d %s", i.Number, i.Title), "", func(o forge.RevertOptions) (*forge.RevertResult, error) {
			return f.CherryPickPR(c, u, r, i, o)
		})), true
	case "m":
		if !i.IsPR {
			return nil, false
		}
		if i.State != "open" {
			return toastErr(fmt.Errorf("pull request is %s", i.State)), true
		}
		warn := ""
		if s.prs != nil && !s.prs.ChecksOK && len(s.prs.Checks) > 0 {
			warn = "checks have not all passed"
		}
		return push(NewForm(s.ctx, fmt.Sprintf("Merge #%d into %s", i.Number, i.PR.BaseBranch), []*Field{
			{Label: "Method", Options: []string{"merge", "squash", "ff"}, Hint: warn},
		}, func(v map[string]string) (tea.Cmd, error) {
			sha, err := f.MergePR(c, u, r, i, v["Method"])
			if err != nil {
				return nil, err
			}
			return tea.Sequence(pop, toast("merged as "+shortSHA(sha))), nil
		})), true
	}
	return nil, false
}

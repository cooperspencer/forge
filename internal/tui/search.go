package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// searchPane is code search: a query box above the matches. scope nil
// searches every repository the user can read.
type searchPane struct {
	ctx    *Ctx
	scope  *store.Repo
	input  textinput.Model
	list   *List
	typing bool
	w, h   int

	q    forge.CodeQuery
	res  *forge.CodeResult
	err  string
	busy bool
	seq  int
}

type searchDoneMsg struct {
	pane *searchPane
	seq  int
	q    forge.CodeQuery
	res  *forge.CodeResult
	err  error
}

const searchHelp = `"exact phrase"  /regex/  path:*.go  path:docs/  repo:name  owner:acme  case:yes  fork:yes`

func newSearchPane(ctx *Ctx, scope *store.Repo, autofocus bool) *searchPane {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 300
	ti.Placeholder = "search code, e.g. ListenAndServe path:*.go"
	cols := []Column{{Title: "REPOSITORY", Flex: 2}, {Title: "FILE", Flex: 3}, {Title: "MATCH", Flex: 5}}
	if scope != nil {
		cols = cols[1:]
	}
	p := &searchPane{ctx: ctx, scope: scope, input: ti, list: NewList(ctx.St, cols...)}
	p.list.Empty = "Type a query and press enter."
	if autofocus {
		p.focusInput()
	}
	return p
}

func (p *searchPane) focusInput() tea.Cmd {
	p.typing = true
	p.list.focused = false
	return p.input.Focus()
}

func (p *searchPane) focusList() {
	p.typing = false
	p.input.Blur()
	p.list.focused = true
}

func (p *searchPane) Load()           {}
func (p *searchPane) Capturing() bool { return p.typing || p.list.Filtering() }
func (p *searchPane) SetSize(w, h int) {
	p.w, p.h = w, h
	p.input.Width = w - 6
	p.list.SetSize(w, max(2, h-5)) // label, input box (3), status
}

func (p *searchPane) Keys() []Key {
	if p.typing {
		return []Key{{"enter", "search"}, {"tab/↓", "results"}, {"esc", "leave the query"}}
	}
	return []Key{{"enter", "open file"}, {"/", "edit query"}, {"↑↓", "select"}}
}

func (p *searchPane) run() tea.Cmd {
	q, err := forge.ParseCodeQuery(p.input.Value())
	if err != nil {
		p.err = err.Error()
		return nil
	}
	p.err, p.busy = "", true
	p.seq++
	seq, ctx, scope := p.seq, p.ctx, p.scope
	return func() tea.Msg {
		res, err := ctx.F.SearchCode(ctx.Ctx, ctx.User, scope, q)
		return searchDoneMsg{pane: p, seq: seq, q: q, res: res, err: err}
	}
}

func (p *searchPane) Update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case searchDoneMsg:
		if msg.pane != p || msg.seq != p.seq {
			return nil, false
		}
		p.busy = false
		if msg.err != nil {
			p.err = msg.err.Error()
			return nil, true
		}
		p.q, p.res = msg.q, msg.res
		p.list.SetRows(p.rows())
		p.list.cursor, p.list.offset = 0, 0
		if len(msg.res.Matches) > 0 {
			p.focusList()
		}
		return nil, true
	case tea.KeyMsg:
		k := msg.String()
		if p.typing {
			switch k {
			case "enter":
				return p.run(), true
			case "tab", "down":
				if len(p.list.rows) > 0 {
					p.focusList()
				}
				return nil, true
			case "esc":
				p.focusList()
				return nil, true
			}
			var cmd tea.Cmd
			p.input, cmd = p.input.Update(msg)
			return cmd, true
		}
		if !p.list.Filtering() {
			switch k {
			case "/", "i":
				return p.focusInput(), true
			case "enter":
				if row := p.list.Selected(); row != nil {
					m := row.Data.(forge.CodeMatch)
					rs := &repoState{ctx: p.ctx, repo: m.Repo, git: p.ctx.F.Git(m.Repo), ref: "refs/heads/" + m.Ref}
					fp := rs.filePageAt(m.Path, m.Line)
					if tp, ok := fp.(*TextPage); ok {
						tp.title = m.Repo.FullName() + ": " + m.Path
					}
					return push(fp), true
				}
				if len(p.list.rows) == 0 {
					return p.focusInput(), true
				}
				return nil, true
			}
		}
		if ok, cmd := p.list.Update(msg); ok {
			return cmd, true
		}
	}
	return nil, false
}

func (p *searchPane) rows() []Row {
	st := p.ctx.St
	re := p.q.Matcher()
	var rows []Row
	prev := ""
	for _, m := range p.res.Matches {
		text := strings.TrimSpace(expandTabs(m.Text))
		// keep the match visible in a narrow column: start a little before it
		if loc := re.FindStringIndex(text); loc != nil && loc[0] > 24 {
			i := loc[0] - 16
			for i > 0 && !utf8.RuneStart(text[i]) {
				i--
			}
			text = "…" + text[i:]
		}
		var b strings.Builder
		last := 0
		for _, loc := range re.FindAllStringIndex(text, -1) {
			if loc[1] == loc[0] {
				continue
			}
			b.WriteString(st.Subtle.Render(text[last:loc[0]]))
			b.WriteString(st.StatusRun.Bold(true).Render(text[loc[0]:loc[1]]))
			last = loc[1]
		}
		b.WriteString(st.Subtle.Render(text[last:]))
		file := st.Accent.Render(m.Path) + st.Faint.Render(fmt.Sprintf(":%d", m.Line))
		cols := []string{file, b.String()}
		if p.scope == nil {
			repo := st.Bold.Render(m.Repo.FullName())
			if m.Repo.FullName() == prev {
				repo = st.Faint.Render(m.Repo.FullName())
			}
			prev = m.Repo.FullName()
			cols = append([]string{repo}, cols...)
		}
		rows = append(rows, Row{Cols: cols, Filter: m.Repo.FullName() + " " + m.Path + " " + m.Text, Data: m})
	}
	if len(rows) == 0 {
		p.list.Empty = "No matches."
	}
	return rows
}

func (p *searchPane) View() string {
	st := p.ctx.St
	box := st.Input
	if p.typing {
		box = st.InputFocus
	}
	where := "all repositories"
	if p.scope != nil {
		where = p.scope.FullName()
	}
	label := st.Label.Render(" ⌕ Search code") + st.Faint.Render(" in "+where)
	if !p.typing && p.input.Value() == "" {
		label += st.Faint.Render("   press / to type a query")
	}
	in := box.Width(min(p.w-2, 100)).Render(p.input.View())
	var status string
	switch {
	case p.err != "":
		status = " " + st.Error.Render("✗ "+p.err)
	case p.busy:
		status = " " + st.StatusRun.Render("searching…")
	case p.res != nil:
		r := p.res
		noun := "matches"
		if len(r.Matches) == 1 {
			noun = "match"
		}
		status = fmt.Sprintf(" %s %s", st.Bold.Render(fmt.Sprint(len(r.Matches))), st.Subtle.Render(noun))
		if p.scope == nil {
			status += st.Subtle.Render(fmt.Sprintf(" in %d of %d repositories", r.Hits, r.Repos))
		}
		status += st.Faint.Render(fmt.Sprintf(" · %.2fs", r.Took.Seconds()))
		if r.Truncated {
			status += "  " + st.StatusRun.Render("(limit reached: narrow it down with path: or repo:)")
		}
	default:
		status = " " + st.Faint.Render(searchHelp)
	}
	status = ansi.Truncate(status, p.w, "…")
	return label + "\n" + in + "\n" + status + "\n" + p.list.View()
}

// NewSearchPage is code search as a page of its own (for one repository).
func NewSearchPage(ctx *Ctx, scope *store.Repo) Page {
	name := "search"
	if scope != nil {
		name = "search " + scope.FullName()
	}
	sp := newSearchPane(ctx, scope, true)
	tp := NewTabPage(ctx, name, []*Tab{{Name: "Code search", Custom: sp}})
	return &focusedPage{TabPage: tp, init: sp.input.Focus}
}

// focusedPage starts the cursor blinking in the search box when opened.
type focusedPage struct {
	*TabPage
	init func() tea.Cmd
}

func (f *focusedPage) Init() tea.Cmd { f.TabPage.Init(); return f.init() }

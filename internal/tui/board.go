package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

// boardPane is the repository's kanban board: one lane per column, issues as
// cards. ←→ pick a column, ↑↓ a card, H/L move the card, K/J reorder it.
type boardPane struct {
	ctx  *Ctx
	repo *store.Repo
	b    *forge.Board
	err  error
	w, h int

	col   int         // selected lane
	sel   map[int]int // selected card per lane
	top   map[int]int // first visible card per lane
	first int         // first visible lane when not all fit
	want  int64       // issue number to select after the next load
	wantC int64       // column id to select after the next load
}

const boardMinLane = 26

func newBoardPane(ctx *Ctx, r *store.Repo) *boardPane {
	return &boardPane{ctx: ctx, repo: r, sel: map[int]int{}, top: map[int]int{}}
}

func (p *boardPane) Load() {
	b, err := p.ctx.F.Board(p.ctx.Ctx, p.ctx.User, p.repo)
	p.b, p.err = b, err
	if err != nil || len(b.Lanes) == 0 {
		return
	}
	if p.want != 0 {
		if li, ci := b.Find(p.want); li >= 0 {
			p.col, p.sel[li] = li, ci
		}
		p.want = 0
	}
	if p.wantC != 0 {
		if li, err := b.Lane(p.wantC, ""); err == nil {
			p.col = li
		}
		p.wantC = 0
	}
	p.col = clamp(p.col, 0, len(b.Lanes)-1)
	for li := range b.Lanes {
		p.sel[li] = clamp(p.sel[li], 0, len(b.Lanes[li].Cards)-1)
	}
}

func clamp(v, lo, hi int) int {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}

func (p *boardPane) SetSize(w, h int) { p.w, p.h = w, h }
func (p *boardPane) Capturing() bool  { return false }

// OwnsKey keeps ←→/h/l for moving between columns instead of switching tabs,
// except at the edges: ← on the first column and → on the last one switch to
// the neighbouring tab as usual, so the board is not a trap.
func (p *boardPane) OwnsKey(k string) bool {
	if p.b == nil || len(p.b.Lanes) == 0 {
		return false
	}
	switch k {
	case "left", "h":
		return p.col > 0
	case "right", "l":
		return p.col < len(p.b.Lanes)-1
	}
	return false
}

func (p *boardPane) Keys() []Key {
	return []Key{{"←→↑↓", "select"}, {"H/L", "move card"}, {"K/J", "reorder"}, {"enter", "open"}, {"n", "new issue"},
		{"a", "add column"}, {"E", "rename column"}, {"D", "delete column"}, {"</>", "move column"}}
}

func (p *boardPane) lane() *forge.BoardLane {
	if p.b == nil || len(p.b.Lanes) == 0 {
		return nil
	}
	return &p.b.Lanes[p.col]
}

func (p *boardPane) card() *store.Issue {
	l := p.lane()
	if l == nil || len(l.Cards) == 0 {
		return nil
	}
	return l.Cards[p.sel[p.col]]
}

// act runs a board change now and reloads, so the selection can follow the
// moved card immediately.
func (p *boardPane) act(fn func() error, ok string) tea.Cmd {
	if err := fn(); err != nil {
		p.want, p.wantC = 0, 0
		return toastErr(err)
	}
	p.Load()
	if ok == "" {
		return nil
	}
	return toast(ok)
}

func (p *boardPane) Update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case refreshMsg:
		p.Load()
		return nil, true
	case tea.KeyMsg:
		return p.key(msg.String())
	}
	return nil, false
}

func (p *boardPane) key(k string) (tea.Cmd, bool) {
	if p.b == nil || len(p.b.Lanes) == 0 {
		return nil, false
	}
	f, c, u, r := p.ctx.F, p.ctx.Ctx, p.ctx.User, p.repo
	lane, card := p.lane(), p.card()
	n := len(lane.Cards)
	signIn := func() (tea.Cmd, bool) { return toastErr(fmt.Errorf("sign in to change the board")), true }
	switch k {
	case "left", "h":
		p.col = clamp(p.col-1, 0, len(p.b.Lanes)-1)
	case "right", "l":
		p.col = clamp(p.col+1, 0, len(p.b.Lanes)-1)
	case "up", "k":
		p.sel[p.col] = clamp(p.sel[p.col]-1, 0, n-1)
	case "down", "j":
		p.sel[p.col] = clamp(p.sel[p.col]+1, 0, n-1)
	case "g", "home":
		p.sel[p.col] = 0
	case "G", "end":
		p.sel[p.col] = clamp(n-1, 0, n-1)
	case "enter":
		if card == nil {
			return nil, true
		}
		return push(NewIssuePage(p.ctx, r, card)), true

	case "H", "L", "shift+left", "shift+right":
		if card == nil {
			return nil, true
		}
		if u == nil {
			return signIn()
		}
		to := p.col + 1
		if k == "H" || k == "shift+left" {
			to = p.col - 1
		}
		if to < 0 || to >= len(p.b.Lanes) {
			return nil, true
		}
		dest := p.b.Lanes[to].Column
		p.want = card.Number
		msg := fmt.Sprintf("#%d → %s", card.Number, dest.Name)
		if p.b.Done(to) {
			msg = fmt.Sprintf("#%d closed", card.Number)
		} else if p.b.Done(p.col) {
			msg = fmt.Sprintf("#%d reopened → %s", card.Number, dest.Name)
		}
		return p.act(func() error { return f.MoveCard(c, u, r, card, dest.ID, 0) }, msg), true
	case "K", "J", "shift+up", "shift+down":
		if card == nil {
			return nil, true
		}
		if u == nil {
			return signIn()
		}
		if p.b.Done(p.col) {
			return toast("the last column is ordered by close time"), true
		}
		to := p.sel[p.col] + 1
		if k == "K" || k == "shift+up" {
			to = p.sel[p.col] - 1
		}
		if to < 0 || to >= n {
			return nil, true
		}
		p.want = card.Number
		return p.act(func() error { return f.MoveCard(c, u, r, card, lane.Column.ID, to) }, ""), true

	case "n":
		if u == nil {
			return toastErr(fmt.Errorf("sign in to participate")), true
		}
		if p.b.Done(p.col) {
			return toastErr(fmt.Errorf("new issues start open: pick another column")), true
		}
		col := lane.Column
		return push(NewForm(p.ctx, fmt.Sprintf("New issue in %s › %s", r.FullName(), col.Name),
			[]*Field{{Label: "Title"}, {Label: "Description", Multiline: true, Placeholder: "Markdown welcome"}},
			func(v map[string]string) (tea.Cmd, error) {
				i, err := f.CreateIssue(c, u, r, v["Title"], v["Description"])
				if err != nil {
					return nil, err
				}
				// place it at the top of the chosen column (the first
				// column already shows new issues on top)
				if err := f.MoveCard(c, u, r, i, col.ID, 0); err != nil && !errors.Is(err, forge.ErrForbidden) {
					return nil, err
				}
				p.want = i.Number
				return tea.Sequence(pop, toast(fmt.Sprintf("opened #%d", i.Number))), nil
			})), true

	case "a":
		if u == nil {
			return signIn()
		}
		return push(NewForm(p.ctx, "Add a column to the board", []*Field{{Label: "Name", Hint: "added before the last (done) column"}},
			func(v map[string]string) (tea.Cmd, error) {
				col, err := f.AddBoardColumn(c, u, r, v["Name"])
				if err != nil {
					return nil, err
				}
				p.wantC = col.ID
				return tea.Sequence(pop, toast("added "+col.Name)), nil
			})), true
	case "E":
		if u == nil {
			return signIn()
		}
		col := lane.Column
		return push(NewForm(p.ctx, "Rename column", []*Field{{Label: "Name", Value: col.Name}},
			func(v map[string]string) (tea.Cmd, error) {
				if err := f.RenameBoardColumn(c, u, r, col.ID, v["Name"]); err != nil {
					return nil, err
				}
				return tea.Sequence(pop, toast("renamed")), nil
			})), true
	case "D":
		if u == nil {
			return signIn()
		}
		col := lane.Column
		q := fmt.Sprintf("Delete the column %q?", col.Name)
		if !p.b.Done(p.col) && n > 0 && p.col != 0 {
			q += fmt.Sprintf(" Its %d card(s) move to %q.", n, p.b.Lanes[0].Column.Name)
		}
		if p.b.Done(p.col) {
			q += " The column before it becomes the done column."
		}
		return push(NewConfirm(p.ctx, q, func() tea.Cmd {
			return p.act(func() error { return f.DeleteBoardColumn(c, u, r, col.ID) }, "deleted "+col.Name)
		})), true
	case "<", ">":
		if u == nil {
			return signIn()
		}
		d := 1
		if k == "<" {
			d = -1
		}
		p.wantC = lane.Column.ID
		return p.act(func() error { return f.MoveBoardColumn(c, u, r, lane.Column.ID, d) }, ""), true
	default:
		return nil, false
	}
	return nil, true
}

// ---------------------------------------------------------------- rendering

var laneColors = []lipgloss.AdaptiveColor{cBlue, cYellow, cAccent, cPink, cTeal, cAccent2}

func laneColor(i, n int) lipgloss.TerminalColor {
	if i == n-1 && n > 1 {
		return cGreen
	}
	return laneColors[i%len(laneColors)]
}

// Header summarises the board above the lanes.
func (p *boardPane) Header() string {
	st := p.ctx.St
	if p.b == nil {
		return ""
	}
	open, done := 0, 0
	for i, l := range p.b.Lanes {
		if p.b.Done(i) {
			done += len(l.Cards) + l.Hidden
		} else {
			open += len(l.Cards)
		}
	}
	s := " " + st.Bold.Render(fmt.Sprint(open)) + st.Subtle.Render(" open") + st.Faint.Render(" · ") +
		st.StatusOK.Render(fmt.Sprint(done)) + st.Subtle.Render(" closed")
	if vis := p.visible(); vis < len(p.b.Lanes) {
		s += st.Faint.Render(fmt.Sprintf("   columns %d–%d of %d", p.first+1, p.first+vis, len(p.b.Lanes)))
	}
	return s
}

func (p *boardPane) visible() int {
	n := len(p.b.Lanes)
	v := (p.w + 1) / (boardMinLane + 1)
	return clamp(v, 1, n)
}

func (p *boardPane) View() string {
	st := p.ctx.St
	if p.err != nil {
		return " " + st.Error.Render(p.err.Error())
	}
	if p.b == nil || len(p.b.Lanes) == 0 {
		return ""
	}
	vis := p.visible()
	if p.col < p.first {
		p.first = p.col
	}
	if p.col >= p.first+vis {
		p.first = p.col - vis + 1
	}
	p.first = clamp(p.first, 0, len(p.b.Lanes)-vis)
	laneW := (p.w - (vis - 1)) / vis
	gap := strings.TrimSuffix(strings.Repeat(" \n", p.h), "\n")
	var parts []string
	for li := p.first; li < p.first+vis; li++ {
		if li > p.first {
			parts = append(parts, gap)
		}
		parts = append(parts, p.renderLane(li, laneW))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func (p *boardPane) renderLane(li, w int) string {
	st := p.ctx.St
	l := p.b.Lanes[li]
	focus := li == p.col
	color := laneColor(li, len(p.b.Lanes))
	name := ansi.Truncate(l.Column.Name, w-8, "…")
	count := len(l.Cards) + l.Hidden
	var head string
	if focus {
		head = st.r.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#111827"}).Background(color).Padding(0, 1).Render(name) +
			" " + st.Subtle.Render(fmt.Sprint(count))
	} else {
		head = st.r.NewStyle().Bold(true).Foreground(color).Padding(0, 1).Render(name) + " " + st.Faint.Render(fmt.Sprint(count))
	}
	rule := st.r.NewStyle().Foreground(color).Render(strings.Repeat("━", w))
	if !focus {
		rule = st.Faint.Render(strings.Repeat("─", w))
	}
	lines := []string{head, rule}
	avail := p.h - len(lines)

	cards := make([][]string, len(l.Cards))
	for ci, is := range l.Cards {
		cards[ci] = strings.Split(p.renderCard(is, w, focus && ci == p.sel[li], color, p.b.Done(li)), "\n")
	}
	sel := p.sel[li]
	top := clamp(p.top[li], 0, max(0, len(cards)-1))
	if sel < top {
		top = sel
	}
	// does top..sel fit, counting the "more" indicators?
	fits := func(top int) bool {
		used := 0
		if top > 0 {
			used++
		}
		for k := top; k <= sel && k < len(cards); k++ {
			used += len(cards[k])
		}
		if sel < len(cards)-1 {
			used++
		}
		return used <= avail
	}
	for top < sel && !fits(top) {
		top++
	}
	p.top[li] = top

	if len(cards) == 0 {
		hint := "no issues"
		if focus && !p.b.Done(li) {
			hint = "no issues · n to add"
		}
		lines = append(lines, "", st.Faint.Italic(true).Render(" "+hint))
	}
	if top > 0 {
		lines = append(lines, st.Faint.Render(fmt.Sprintf(" ↑ %d more", top)))
	}
	for k := top; k < len(cards); k++ {
		room := p.h - len(lines)
		if k < len(cards)-1 || l.Hidden > 0 {
			room-- // keep a line for "↓ more"
		}
		if len(cards[k]) > room {
			rest := len(cards) - k + l.Hidden
			lines = append(lines, st.Faint.Render(fmt.Sprintf(" ↓ %d more", rest)))
			break
		}
		lines = append(lines, cards[k]...)
		if k == len(cards)-1 && l.Hidden > 0 && len(lines) < p.h {
			lines = append(lines, st.Faint.Render(fmt.Sprintf(" + %d older closed", l.Hidden)))
		}
	}
	if len(lines) > p.h {
		lines = lines[:p.h]
	}
	for i, ln := range lines {
		if pad := w - ansi.StringWidth(ln); pad > 0 {
			lines[i] = ln + strings.Repeat(" ", pad)
		} else if pad < 0 {
			lines[i] = ansi.Truncate(ln, w, "")
		}
	}
	for len(lines) < p.h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

// renderCard draws one issue card: number and title (up to two lines), then
// labels and author.
func (p *boardPane) renderCard(i *store.Issue, w int, selected bool, color lipgloss.TerminalColor, done bool) string {
	st := p.ctx.St
	inner := w - 4
	if inner < 4 {
		inner = 4
	}
	num := fmt.Sprintf("#%d ", i.Number)
	title := ansi.Wrap(num+i.Title, inner, "")
	tl := strings.Split(title, "\n")
	if len(tl) > 2 {
		tl = []string{tl[0], ansi.Truncate(strings.Join(tl[1:], " "), inner, "…")}
	}
	titleStyle := st.Title
	if done {
		titleStyle = st.Subtle
	}
	for k, t := range tl {
		if k == 0 && strings.HasPrefix(t, num) {
			icon := ""
			if done {
				icon = st.StatusOK.Render("✓") + " "
			}
			tl[k] = icon + st.Faint.Render(strings.TrimSuffix(num, " ")) + " " + titleStyle.Render(strings.TrimPrefix(t, num))
			if done {
				tl[k] = ansi.Truncate(tl[k], inner, "…")
			}
		} else {
			tl[k] = titleStyle.Render(t)
		}
	}
	var meta []string
	for _, lb := range i.Labels {
		meta = append(meta, st.Label.Render(lb))
	}
	who := st.Subtle.Render(i.Author)
	if done {
		who = st.Faint.Render("closed " + ago(i.UpdatedAt))
	}
	meta = append(meta, who)
	if i.Comments > 0 {
		meta = append(meta, st.Faint.Render(fmt.Sprintf("💬%d", i.Comments)))
	}
	body := strings.Join(tl, "\n") + "\n" + ansi.Truncate(strings.Join(meta, st.Faint.Render(" · ")), inner, "…")

	box := st.r.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1).Width(w - 2)
	if selected {
		box = box.Border(lipgloss.ThickBorder()).BorderForeground(color)
	}
	return box.Render(body)
}

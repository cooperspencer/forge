package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Tab is one pane of a TabPage. Most tabs are lists; a tab may instead supply
// a custom view (Custom) that handles its own keys.
type Tab struct {
	Name string
	List *List
	Load func() []Row
	Keys []Key
	// Handle reacts to a key on the selected row (row may be nil). Return
	// handled=false to let the key fall through.
	Handle func(key string, row *Row) (tea.Cmd, bool)
	Custom Pane
	// Header is optional text rendered above the list.
	Header func() string
	// Below is optional content under the list (a directory's README); when
	// it is non-empty the list shrinks to its rows (at most half the space)
	// and Below gets the rest, cut off with BelowMore if it doesn't fit.
	Below     func(w int) string
	BelowMore string

	belowH int // lines available for Below (set by SetSize)
}

// Pane is a custom tab body.
type Pane interface {
	Load()
	Update(tea.Msg) (tea.Cmd, bool)
	View() string
	SetSize(w, h int)
	Keys() []Key
	Capturing() bool
}

type TabPage struct {
	ctx    *Ctx
	title  string
	tabs   []*Tab
	t      Tabs
	w, h   int
	top    func() string // optional summary line(s) above tabs
	loaded map[int]bool

	// global handles keys not consumed by the active tab.
	global     func(key string) (tea.Cmd, bool)
	globalKeys []Key
}

func NewTabPage(ctx *Ctx, title string, tabs []*Tab) *TabPage {
	p := &TabPage{ctx: ctx, title: title, tabs: tabs, loaded: map[int]bool{}}
	for _, t := range tabs {
		p.t.Names = append(p.t.Names, t.Name)
	}
	return p
}

func (p *TabPage) Title() string { return p.title }
func (p *TabPage) cur() *Tab     { return p.tabs[p.t.Active] }

func (p *TabPage) Capturing() bool {
	t := p.cur()
	if t.Custom != nil {
		return t.Custom.Capturing()
	}
	return t.List != nil && t.List.Filtering()
}

func (p *TabPage) Keys() []Key {
	t := p.cur()
	if t.Custom != nil {
		return t.Custom.Keys()
	}
	return append(append(append([]Key{}, t.Keys...), p.globalKeys...), Key{"/", "filter"})
}

func (p *TabPage) Init() tea.Cmd { p.reload(); return nil }

func (p *TabPage) reload() {
	t := p.cur()
	if t.Custom != nil {
		t.Custom.Load()
	} else if t.Load != nil && t.List != nil {
		t.List.SetRows(t.Load())
		if t.Below != nil && p.w > 0 {
			p.SetSize(p.w, p.h) // the list's height depends on its rows
		}
	}
	p.loaded[p.t.Active] = true
}

// fit truncates every line to the page width so headers never wrap and
// break the height budget.
func (p *TabPage) fit(s string) string {
	if p.w <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if ansi.StringWidth(l) > p.w {
			lines[i] = ansi.Truncate(l, p.w, "…")
		}
	}
	return strings.Join(lines, "\n")
}

func (p *TabPage) SetSize(w, h int) {
	p.w, p.h = w, h
	for i, t := range p.tabs {
		bh := h - 2
		if p.top != nil {
			bh -= lipgloss.Height(p.top())
		}
		if t.Header != nil && i == p.t.Active {
			bh -= lipgloss.Height(t.Header())
		}
		if bh < 2 {
			bh = 2
		}
		t.belowH = 0
		if t.List != nil && t.Below != nil && i == p.t.Active {
			if below := t.Below(w); below != "" {
				rows := t.List.Len()
				if t.List.Filtering() || t.List.filter.Value() != "" {
					rows++
				}
				if !t.List.NoHeader {
					rows++
				}
				listH := min(max(rows, 1), max(bh/2, 3))
				if bh-listH-1 >= 3 {
					t.belowH = bh - listH - 1 // one line separates list and README
					bh = listH
				}
			}
		}
		if t.List != nil {
			t.List.SetSize(w, bh)
		}
		if t.Custom != nil {
			t.Custom.SetSize(w, bh)
		}
	}
}

func (p *TabPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	t := p.cur()
	switch msg := msg.(type) {
	case refreshMsg, tickMsg:
		if _, ok := msg.(tickMsg); ok && t.Custom == nil {
			return p, nil
		}
		if t.Custom != nil {
			if cmd, ok := t.Custom.Update(msg); ok {
				return p, cmd
			}
		}
		if _, ok := msg.(refreshMsg); ok {
			p.reload()
		}
		return p, nil
	case tea.KeyMsg:
		owned := false
		if o, ok := t.Custom.(interface{ OwnsKey(string) bool }); ok {
			owned = o.OwnsKey(msg.String())
		}
		if !p.Capturing() && !owned {
			if p.t.Update(msg) {
				p.SetSize(p.w, p.h)
				p.reload()
				return p, nil
			}
		}
		if !p.Capturing() {
			if msg.String() == "r" {
				p.reload()
				return p, toast("refreshed")
			}
		}
		if t.Custom != nil {
			if cmd, ok := t.Custom.Update(msg); ok {
				return p, cmd
			}
		}
		if t.List != nil {
			if ok, cmd := t.List.Update(msg); ok {
				return p, cmd
			}
		}
		if t.Handle != nil {
			var row *Row
			if t.List != nil {
				row = t.List.Selected()
			}
			if cmd, ok := t.Handle(msg.String(), row); ok {
				return p, cmd
			}
		}
		if p.global != nil && !p.Capturing() {
			if cmd, ok := p.global(msg.String()); ok {
				return p, cmd
			}
		}
	default:
		if t.Custom != nil {
			cmd, _ := t.Custom.Update(msg)
			return p, cmd
		}
	}
	return p, nil
}

func (p *TabPage) View() string {
	var b strings.Builder
	if p.top != nil {
		b.WriteString(p.fit(p.top()) + "\n")
	}
	b.WriteString(p.t.View(p.ctx.St, p.w) + "\n")
	b.WriteString(p.ctx.St.Faint.Render(strings.Repeat("─", p.w)) + "\n")
	t := p.cur()
	if t.Header != nil {
		b.WriteString(p.fit(t.Header()) + "\n")
	}
	if t.Custom != nil {
		b.WriteString(t.Custom.View())
	} else if t.List != nil {
		b.WriteString(t.List.View())
		if t.belowH > 0 {
			lines := strings.Split(strings.TrimRight(t.Below(p.w), "\n"), "\n")
			if len(lines) > t.belowH {
				lines = append(lines[:t.belowH-1], p.ctx.St.Faint.Render("  "+t.BelowMore))
			}
			b.WriteString("\n" + p.ctx.St.Faint.Render(strings.Repeat("─", p.w)) + "\n" + p.fit(strings.Join(lines, "\n")))
		}
	}
	return b.String()
}

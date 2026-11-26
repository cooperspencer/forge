package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// TextPage shows scrollable pre-rendered content (files, diffs, messages).
type TextPage struct {
	ctx     *Ctx
	title   string
	content string
	vp      viewport.Model
	keys    []Key
	handle  func(key string) (tea.Cmd, bool)
	gutter  bool
	reload  func() string
	// renderW, if set, produces the content for a given width (Markdown
	// re-wraps on resize) and is also called on refresh.
	renderW func(w int) string
	// mark highlights this line number (with gutter) and scrolls to it once.
	mark     int
	scrolled bool
}

func NewTextPage(ctx *Ctx, title, content string) *TextPage {
	return &TextPage{ctx: ctx, title: title, content: content, vp: viewport.New(80, 20)}
}

func (t *TextPage) Title() string { return t.title }
func (t *TextPage) Init() tea.Cmd { return nil }
func (t *TextPage) Keys() []Key {
	return append(append([]Key{}, t.keys...), Key{"↑↓/pgup/pgdn", "scroll"}, Key{"g/G", "top/bottom"})
}

func (t *TextPage) SetSize(w, h int) {
	t.vp.Width, t.vp.Height = w, h-1
	if t.renderW != nil {
		t.content = t.renderW(w)
	}
	t.render()
}

func (t *TextPage) render() {
	content := t.content
	if t.gutter {
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		wd := len(fmt.Sprint(len(lines)))
		for i, l := range lines {
			if i+1 == t.mark {
				lines[i] = t.ctx.St.Label.Render(fmt.Sprintf("%*d ▶ ", wd, i+1)) + l
				continue
			}
			lines[i] = t.ctx.St.Faint.Render(fmt.Sprintf("%*d │ ", wd, i+1)) + l
		}
		content = strings.Join(lines, "\n")
	}
	// hard-wrap overly long lines so nothing spills past the terminal
	var out []string
	for _, l := range strings.Split(content, "\n") {
		if ansi.StringWidth(l) > t.vp.Width && t.vp.Width > 0 {
			out = append(out, ansi.Hardwrap(l, t.vp.Width, true))
		} else {
			out = append(out, l)
		}
	}
	t.vp.SetContent(strings.Join(out, "\n"))
	if t.mark > 0 && !t.scrolled && t.vp.Height > 0 {
		t.scrolled = true
		t.vp.SetYOffset(max(0, t.mark-1-t.vp.Height/3)) // the line a third down the screen
	}
}

func (t *TextPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case refreshMsg:
		if t.renderW != nil {
			t.content = t.renderW(t.vp.Width)
			t.render()
		} else if t.reload != nil {
			t.content = t.reload()
			t.render()
		}
		return t, nil
	case tea.KeyMsg:
		if t.handle != nil {
			if cmd, ok := t.handle(msg.String()); ok {
				return t, cmd
			}
		}
		switch msg.String() {
		case "g", "home":
			t.vp.GotoTop()
			return t, nil
		case "G", "end":
			t.vp.GotoBottom()
			return t, nil
		}
	}
	var cmd tea.Cmd
	t.vp, cmd = t.vp.Update(msg)
	return t, cmd
}

func (t *TextPage) View() string {
	pct := fmt.Sprintf("%3.0f%%", t.vp.ScrollPercent()*100)
	status := t.ctx.St.Faint.Render(lipgloss.PlaceHorizontal(t.vp.Width, lipgloss.Right, pct))
	return t.vp.View() + "\n" + status
}

// wrap soft-wraps plain text to width for comment bodies etc.
func wrap(s string, w int) string {
	if w < 20 {
		w = 20
	}
	return ansi.Wordwrap(s, w, "")
}

// indent prefixes every line.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

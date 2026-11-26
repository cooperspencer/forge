package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Row is one entry of a List. Cols are pre-styled strings; Filter is the
// plain text matched by "/" search (defaults to the joined columns).
type Row struct {
	Cols   []string
	Filter string
	Data   any
}

// Column describes layout: fixed Width, or Flex share of remaining space.
type Column struct {
	Title string
	Width int
	Flex  int
	Right bool
	// HideBelow drops the column when the list is narrower than this, so
	// the important (flex) columns keep their room on small terminals.
	HideBelow int
}

// List is a keyboard-driven, filterable, column-aligned list.
type List struct {
	st      *Styles
	cols    []Column
	rows    []Row
	visible []int
	cursor  int
	offset  int
	w, h    int
	focused bool

	filtering bool
	filter    textinput.Model
	Empty     string
	NoHeader  bool
}

func NewList(st *Styles, cols ...Column) *List {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter…"
	return &List{st: st, cols: cols, filter: ti, focused: true, Empty: "Nothing here yet."}
}

func (l *List) SetRows(rows []Row) {
	var sel any
	if r := l.Selected(); r != nil {
		sel = r.Filter + strings.Join(r.Cols, "")
	}
	l.rows = rows
	l.applyFilter()
	// try to keep the cursor on the same item after refresh
	if sel != nil {
		for i, idx := range l.visible {
			if l.rows[idx].Filter+strings.Join(l.rows[idx].Cols, "") == sel {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

func (l *List) Len() int         { return len(l.visible) }
func (l *List) SetSize(w, h int) { l.w, l.h = w, h; l.clamp() }
func (l *List) Focus(on bool)    { l.focused = on }
func (l *List) Filtering() bool  { return l.filtering }
func (l *List) Cursor() int      { return l.cursor }
func (l *List) SetCursor(i int)  { l.cursor = i; l.clamp() }

func (l *List) Selected() *Row {
	if l.cursor < 0 || l.cursor >= len(l.visible) {
		return nil
	}
	return &l.rows[l.visible[l.cursor]]
}

func (l *List) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(l.filter.Value()))
	l.visible = l.visible[:0]
	for i, r := range l.rows {
		text := r.Filter
		if text == "" {
			text = ansi.Strip(strings.Join(r.Cols, " "))
		}
		if q == "" || fuzzy(strings.ToLower(text), q) {
			l.visible = append(l.visible, i)
		}
	}
}

// fuzzy matches if all space-separated terms appear as substrings.
func fuzzy(text, q string) bool {
	for _, t := range strings.Fields(q) {
		if !strings.Contains(text, t) {
			return false
		}
	}
	return true
}

func (l *List) bodyHeight() int {
	h := l.h
	if !l.NoHeader {
		h--
	}
	if l.filtering || l.filter.Value() != "" {
		h--
	}
	if h < 1 {
		h = 1
	}
	return h
}

func (l *List) clamp() {
	if l.cursor >= len(l.visible) {
		l.cursor = len(l.visible) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	bh := l.bodyHeight()
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+bh {
		l.offset = l.cursor - bh + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

// Update handles navigation and filter input. It reports whether the key was consumed.
func (l *List) Update(msg tea.Msg) (bool, tea.Cmd) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return false, nil
	}
	if l.filtering {
		switch km.String() {
		case "esc":
			l.filtering = false
			l.filter.SetValue("")
			l.filter.Blur()
			l.applyFilter()
			l.clamp()
			return true, nil
		case "enter", "down", "up", "ctrl+n", "ctrl+p":
			l.filtering = false
			l.filter.Blur()
			if km.String() == "enter" {
				return true, nil
			}
		default:
			var cmd tea.Cmd
			l.filter, cmd = l.filter.Update(msg)
			l.applyFilter()
			l.cursor = 0
			l.clamp()
			return true, cmd
		}
	}
	switch km.String() {
	case "up", "k", "ctrl+p":
		l.cursor--
	case "down", "j", "ctrl+n":
		l.cursor++
	case "pgup", "ctrl+u":
		l.cursor -= l.bodyHeight() / 2
	case "pgdown", "ctrl+d":
		l.cursor += l.bodyHeight() / 2
	case "home", "g":
		l.cursor = 0
	case "end", "G":
		l.cursor = len(l.visible) - 1
	case "/":
		l.filtering = true
		return true, l.filter.Focus()
	case "esc":
		if l.filter.Value() != "" {
			l.filter.SetValue("")
			l.applyFilter()
			l.clamp()
			return true, nil
		}
		return false, nil
	default:
		return false, nil
	}
	l.clamp()
	return true, nil
}

// hidden reports whether column i is dropped at the current width.
func (l *List) hidden(i int) bool { return l.cols[i].HideBelow > 0 && l.w < l.cols[i].HideBelow }

func (l *List) widths() []int {
	ws := make([]int, len(l.cols))
	fixed, flex, shown := 0, 0, 0
	for i, c := range l.cols {
		if l.hidden(i) {
			continue
		}
		shown++
		ws[i] = c.Width
		fixed += c.Width
		flex += c.Flex
	}
	gaps := shown - 1
	rem := l.w - fixed - gaps - 2
	if flex > 0 && rem > 0 {
		for i, c := range l.cols {
			if c.Flex > 0 && !l.hidden(i) {
				ws[i] = rem * c.Flex / flex
			}
		}
	}
	return ws
}

func cell(s string, w int, right bool) string {
	if w <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	pad := w - ansi.StringWidth(s)
	if right {
		return strings.Repeat(" ", pad) + s
	}
	return s + strings.Repeat(" ", pad)
}

func (l *List) View() string {
	var b strings.Builder
	ws := l.widths()
	if l.filtering || l.filter.Value() != "" {
		b.WriteString(" " + l.filter.View() + "\n")
	}
	if !l.NoHeader {
		var parts []string
		for i, c := range l.cols {
			if !l.hidden(i) {
				parts = append(parts, cell(c.Title, ws[i], c.Right))
			}
		}
		b.WriteString(l.st.ColHeader.Render(" "+strings.Join(parts, " ")) + "\n")
	}
	bh := l.bodyHeight()
	if len(l.visible) == 0 {
		b.WriteString(l.st.Faint.Render("  " + l.Empty))
		return lipgloss.NewStyle().Height(l.h).MaxHeight(l.h).Render(b.String())
	}
	for i := l.offset; i < len(l.visible) && i < l.offset+bh; i++ {
		r := l.rows[l.visible[i]]
		var parts []string
		for c := range l.cols {
			if l.hidden(c) {
				continue
			}
			v := ""
			if c < len(r.Cols) {
				v = r.Cols[c]
			}
			parts = append(parts, cell(v, ws[c], l.cols[c].Right))
		}
		line := " " + strings.Join(parts, " ") + " "
		if i == l.cursor {
			// strip inner styling so the selection background is continuous
			plain := cell(ansi.Strip(line), l.w, false)
			if l.focused {
				line = l.st.Selected.Render(plain)
			} else {
				line = l.st.SelectedBlur.Render(plain)
			}
		}
		b.WriteString(line)
		if i < l.offset+bh-1 && i < len(l.visible)-1 {
			b.WriteByte('\n')
		}
	}
	return lipgloss.NewStyle().Height(l.h).MaxHeight(l.h).Render(b.String())
}

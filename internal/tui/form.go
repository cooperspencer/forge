package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Field is one input of a Form.
type Field struct {
	Label       string
	Value       string
	Placeholder string
	Multiline   bool
	Tall        bool // multiline field that takes all the room left (editors)
	Secret      bool
	Options     []string // if set, the field is a choice cycled with ←/→
	// Checkbox makes the field an on/off toggle (space). Value "true" starts
	// it checked, Placeholder is the text beside the box, and the submitted
	// value is "true" or "false".
	Checkbox bool
	Hint     string
	// OnChange runs when a choice or checkbox changes (not for text); it may
	// update other fields with FormPage.Set.
	OnChange func(f *FormPage, value string)

	ti      textinput.Model
	ta      textarea.Model
	opt     int
	checked bool
}

// FormPage is a generic modal form. Submit receives values by label and
// returns a command (usually async work) — or an error to display inline.
type FormPage struct {
	ctx    *Ctx
	title  string
	fields []*Field
	focus  int
	err    string
	submit func(vals map[string]string) (tea.Cmd, error)
	w, h   int
}

func NewForm(ctx *Ctx, title string, fields []*Field, submit func(map[string]string) (tea.Cmd, error)) *FormPage {
	f := &FormPage{ctx: ctx, title: title, fields: fields, submit: submit}
	for _, fl := range fields {
		if fl.Checkbox {
			fl.checked = fl.Value == "true"
		} else if fl.Multiline {
			ta := textarea.New()
			ta.SetValue(fl.Value)
			ta.Placeholder = fl.Placeholder
			ta.ShowLineNumbers = false
			ta.CharLimit = 0
			ta.Prompt = ""
			fl.ta = ta
		} else if fl.Options == nil {
			ti := textinput.New()
			ti.SetValue(fl.Value)
			ti.Placeholder = fl.Placeholder
			ti.Prompt = ""
			ti.CharLimit = 0
			if fl.Secret {
				ti.EchoMode = textinput.EchoPassword
			}
			fl.ti = ti
		} else {
			for i, o := range fl.Options {
				if o == fl.Value {
					fl.opt = i
				}
			}
		}
	}
	return f
}

func (f *FormPage) Title() string   { return f.title }
func (f *FormPage) Capturing() bool { return true }
func (f *FormPage) SetSize(w, h int) {
	f.w, f.h = w, h
	inner := w - 8
	if inner > 100 {
		inner = 100
	}
	multi := 0
	for _, fl := range f.fields {
		if fl.Multiline {
			multi++
		}
	}
	// leave room for single-line fields (3 rows each + label)
	avail := h - 6 - len(f.fields)*4
	for _, fl := range f.fields {
		if fl.Multiline {
			th := 6
			if multi > 0 && avail/multi > th {
				th = avail / multi
			}
			if th > 20 && !fl.Tall {
				th = 20
			}
			fl.ta.SetWidth(inner)
			fl.ta.SetHeight(th)
		} else {
			fl.ti.Width = inner - 2
		}
	}
}

func (f *FormPage) Keys() []Key {
	return []Key{{"tab", "next field"}, {"ctrl+s", "submit"}, {"esc", "cancel"}}
}

func (f *FormPage) Init() tea.Cmd { return f.focusCmd() }

func (f *FormPage) focusCmd() tea.Cmd {
	var cmd tea.Cmd
	for i, fl := range f.fields {
		switch {
		case fl.Multiline:
			if i == f.focus {
				cmd = fl.ta.Focus()
			} else {
				fl.ta.Blur()
			}
		case fl.Checkbox:
		case fl.Options == nil:
			if i == f.focus {
				cmd = fl.ti.Focus()
			} else {
				fl.ti.Blur()
			}
		}
	}
	return cmd
}

func (f *FormPage) values() map[string]string {
	v := map[string]string{}
	for _, fl := range f.fields {
		switch {
		case fl.Checkbox:
			v[fl.Label] = "false"
			if fl.checked {
				v[fl.Label] = "true"
			}
		case fl.Multiline:
			v[fl.Label] = fl.ta.Value()
		case fl.Options != nil:
			v[fl.Label] = fl.Options[fl.opt]
		default:
			v[fl.Label] = strings.TrimSpace(fl.ti.Value())
		}
	}
	return v
}

type formErrMsg struct{ err error }

func (f *FormPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	switch msg := msg.(type) {
	case formErrMsg:
		f.err = msg.err.Error()
		return f, nil
	case tea.KeyMsg:
		cur := f.fields[f.focus]
		switch msg.String() {
		case "esc":
			return f, pop
		case "tab", "shift+tab":
			if msg.String() == "tab" {
				f.focus = (f.focus + 1) % len(f.fields)
			} else {
				f.focus = (f.focus + len(f.fields) - 1) % len(f.fields)
			}
			return f, f.focusCmd()
		case "ctrl+s":
			cmd, err := f.submit(f.values())
			if err != nil {
				f.err = err.Error()
				return f, nil
			}
			return f, cmd
		case "enter":
			if !cur.Multiline {
				if f.focus == len(f.fields)-1 {
					cmd, err := f.submit(f.values())
					if err != nil {
						f.err = err.Error()
						return f, nil
					}
					return f, cmd
				}
				f.focus++
				return f, f.focusCmd()
			}
		case "left", "right", " ", "x":
			if cur.Checkbox {
				cur.checked = !cur.checked
				f.changed(cur)
				return f, nil
			}
			if cur.Options != nil && msg.String() != "x" {
				if msg.String() == "left" {
					cur.opt = (cur.opt + len(cur.Options) - 1) % len(cur.Options)
				} else {
					cur.opt = (cur.opt + 1) % len(cur.Options)
				}
				f.changed(cur)
				return f, nil
			}
		}
		var cmd tea.Cmd
		if cur.Multiline {
			cur.ta, cmd = cur.ta.Update(msg)
		} else if cur.Options == nil && !cur.Checkbox {
			cur.ti, cmd = cur.ti.Update(msg)
		}
		return f, cmd
	}
	return f, nil
}

func (f *FormPage) changed(fl *Field) {
	if fl.OnChange != nil {
		fl.OnChange(f, f.values()[fl.Label])
	}
}

// Set changes a field's value (a choice, "true"/"false" for checkboxes, or
// text) without running its OnChange.
func (f *FormPage) Set(label, value string) {
	for _, fl := range f.fields {
		if fl.Label != label {
			continue
		}
		switch {
		case fl.Checkbox:
			fl.checked = value == "true"
		case fl.Options != nil:
			for i, o := range fl.Options {
				if o == value {
					fl.opt = i
				}
			}
		case fl.Multiline:
			fl.ta.SetValue(value)
		default:
			fl.ti.SetValue(value)
		}
	}
}

func (f *FormPage) View() string {
	st := f.ctx.St
	var b strings.Builder
	b.WriteString(st.Title.Render(f.title) + "\n\n")
	var focusTop, focusBottom int // lines of the focused field
	for i, fl := range f.fields {
		if i == f.focus {
			focusTop = strings.Count(b.String(), "\n")
		}
		lbl := st.Subtle.Render(fl.Label)
		if i == f.focus {
			lbl = st.Label.Render(fl.Label)
		}
		if fl.Hint != "" {
			lbl += "  " + st.Faint.Render(fl.Hint)
		}
		b.WriteString(lbl + "\n")
		box := st.Input
		if i == f.focus {
			box = st.InputFocus
		}
		switch {
		case fl.Checkbox:
			mark := st.Faint.Render("[ ]")
			if fl.checked {
				mark = st.StatusOK.Render("[✓]")
			}
			text := fl.Placeholder
			if i == f.focus {
				text += st.Faint.Render("   space to toggle")
			}
			b.WriteString(box.Render(mark + " " + text))
		case fl.Multiline:
			b.WriteString(box.Render(fl.ta.View()))
		case fl.Options != nil:
			var opts []string
			for j, o := range fl.Options {
				if j == fl.opt {
					opts = append(opts, st.Selected.Render(" "+o+" "))
				} else {
					opts = append(opts, st.Subtle.Render(" "+o+" "))
				}
			}
			b.WriteString(box.Render(strings.Join(opts, " ")))
		default:
			b.WriteString(box.Render(fl.ti.View()))
		}
		b.WriteString("\n")
		if i == f.focus {
			focusBottom = strings.Count(b.String(), "\n")
		}
	}
	// taller than the screen: scroll so the focused field is visible
	if avail := f.h - 3; avail > 6 {
		lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
		if len(lines) > avail {
			start := 0
			if focusBottom > avail-1 {
				start = focusBottom - avail + 1
			}
			start = min(start, max(0, focusTop-1))
			end := min(len(lines), start+avail)
			win := append([]string{}, lines[start:end]...)
			if start > 0 {
				win[0] = st.Faint.Render("  ↑ more fields")
			}
			if end < len(lines) {
				win[len(win)-1] = st.Faint.Render("  ↓ more fields (tab)")
			}
			b.Reset()
			b.WriteString(strings.Join(win, "\n") + "\n")
		}
	}
	if f.err != "" {
		b.WriteString("\n" + st.Error.Render("✗ "+f.err))
	}
	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
}

// ConfirmPage asks a yes/no question.
type ConfirmPage struct {
	ctx      *Ctx
	question string
	onYes    func() tea.Cmd
}

func NewConfirm(ctx *Ctx, q string, onYes func() tea.Cmd) *ConfirmPage {
	return &ConfirmPage{ctx: ctx, question: q, onYes: onYes}
}

func (c *ConfirmPage) Title() string    { return "Confirm" }
func (c *ConfirmPage) Init() tea.Cmd    { return nil }
func (c *ConfirmPage) SetSize(w, h int) {}
func (c *ConfirmPage) Capturing() bool  { return true }
func (c *ConfirmPage) Keys() []Key      { return []Key{{"y", "yes"}, {"n/esc", "no"}} }
func (c *ConfirmPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "y", "Y":
			return c, tea.Sequence(pop, c.onYes())
		case "n", "N", "esc", "q":
			return c, pop
		}
	}
	return c, nil
}
func (c *ConfirmPage) View() string {
	return lipgloss.NewStyle().Padding(2, 4).Render(
		c.ctx.St.BoxFocus.Padding(1, 3).Render(c.ctx.St.Title.Render(c.question) + "\n\n" +
			c.ctx.St.Key.Render("y") + c.ctx.St.KeyDesc.Render(" confirm   ") + c.ctx.St.Key.Render("n") + c.ctx.St.KeyDesc.Render(" cancel")))
}

// confirmName checks a "type the name to confirm" answer: the short name
// ("app") or the full one ("acme/app") both count.
func confirmName(input, name, full string) bool {
	input = strings.TrimSpace(input)
	return input != "" && (input == name || input == full)
}

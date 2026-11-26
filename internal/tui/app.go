// Package tui is the primary interface of forge: a Bubble Tea application
// served over SSH. Each session gets its own App with the user's identity.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// Ctx is shared by all pages of a session.
type Ctx struct {
	F      *forge.Forge
	User   *store.User // nil = anonymous
	PubKey gossh.PublicKey
	St     *Styles
	Ctx    context.Context
}

func (c *Ctx) Access(r *store.Repo) store.Access { return c.F.Store.AccessLevel(c.Ctx, c.User, r) }

// Key is a footer hint.
type Key struct{ Key, Desc string }

// Page is one screen on the navigation stack.
type Page interface {
	Init() tea.Cmd
	Update(tea.Msg) (Page, tea.Cmd)
	View() string
	Title() string
	Keys() []Key
	SetSize(w, h int)
}

// capturer pages consume all keys (text entry), so global shortcuts are disabled.
type capturer interface{ Capturing() bool }

// ---------------------------------------------------------------- messages

type pushMsg struct{ p Page }
type popMsg struct{}
type replaceMsg struct{ p Page }
type toastMsg struct {
	text string
	err  bool
}
type refreshMsg struct{ topic string }
type tickMsg time.Time

func push(p Page) tea.Cmd    { return func() tea.Msg { return pushMsg{p} } }
func replace(p Page) tea.Cmd { return func() tea.Msg { return replaceMsg{p} } }
func pop() tea.Msg           { return popMsg{} }
func toast(s string) tea.Cmd { return func() tea.Msg { return toastMsg{text: s} } }
func toastErr(err error) tea.Cmd {
	return func() tea.Msg {
		if errors.Is(err, forge.ErrForbidden) {
			return toastMsg{text: "permission denied", err: true}
		}
		return toastMsg{text: err.Error(), err: true}
	}
}

// do runs fn asynchronously and reports the outcome as a toast; on success it
// optionally follows with `then` (e.g. pop + refresh).
func do(fn func() error, ok string, then ...tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		if err := fn(); err != nil {
			if errors.Is(err, forge.ErrForbidden) {
				return toastMsg{text: "permission denied", err: true}
			}
			return toastMsg{text: err.Error(), err: true}
		}
		return batchMsg(append([]tea.Cmd{toast(ok)}, then...))
	}
}

type batchMsg []tea.Cmd

// ---------------------------------------------------------------- app

type App struct {
	ctx    *Ctx
	stack  []Page
	w, h   int
	toast  string
	toastE bool
	toastT time.Time
	help   bool
	sub    <-chan string
	unsub  func()
	unread int
}

func New(ctx *Ctx, first Page) *App {
	a := &App{ctx: ctx, stack: []Page{first}}
	a.sub, a.unsub = ctx.F.Broker.Subscribe()
	return a
}

type closer interface{ Close() }

func closePage(p Page) {
	if c, ok := p.(closer); ok {
		c.Close()
	}
}

func (a *App) Close() {
	for _, p := range a.stack {
		closePage(p)
	}
	if a.unsub != nil {
		a.unsub()
	}
}

func (a *App) top() Page { return a.stack[len(a.stack)-1] }

func (a *App) waitBroker() tea.Cmd {
	return func() tea.Msg {
		topic, ok := <-a.sub
		if !ok {
			return nil
		}
		return refreshMsg{topic}
	}
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (a *App) Init() tea.Cmd {
	a.refreshUnread()
	return tea.Batch(a.top().Init(), a.waitBroker(), tick())
}

func (a *App) refreshUnread() {
	if a.ctx.User != nil {
		a.unread = a.ctx.F.Store.UnreadCount(a.ctx.Ctx, a.ctx.User.ID)
	}
}

const headerH, footerH = 2, 2

func (a *App) contentSize() (int, int) {
	h := a.h - headerH - footerH
	if h < 3 {
		h = 3
	}
	return a.w, h
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.w, a.h = msg.Width, msg.Height
		w, h := a.contentSize()
		for _, p := range a.stack {
			p.SetSize(w, h)
		}
		return a, nil
	case batchMsg:
		return a, tea.Sequence(msg...)
	case pushMsg:
		w, h := a.contentSize()
		msg.p.SetSize(w, h)
		a.stack = append(a.stack, msg.p)
		return a, msg.p.Init()
	case replaceMsg:
		w, h := a.contentSize()
		msg.p.SetSize(w, h)
		a.stack[len(a.stack)-1] = msg.p
		return a, msg.p.Init()
	case popMsg:
		if len(a.stack) > 1 {
			closePage(a.top())
			a.stack = a.stack[:len(a.stack)-1]
			// let the revealed page reload its data
			return a, a.forward(refreshMsg{"pop"})
		}
		return a, nil
	case toastMsg:
		a.toast, a.toastE, a.toastT = msg.text, msg.err, time.Now()
		return a, nil
	case refreshMsg:
		a.refreshUnread()
		return a, tea.Batch(a.forward(msg), a.waitBroker())
	case tickMsg:
		if a.toast != "" && time.Since(a.toastT) > 4*time.Second {
			a.toast = ""
		}
		return a, tea.Batch(a.forward(msg), tick())
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
		if a.help {
			a.help = false
			return a, nil
		}
		capturing := false
		if c, ok := a.top().(capturer); ok {
			capturing = c.Capturing()
		}
		if !capturing {
			switch msg.String() {
			case "?":
				a.help = true
				return a, nil
			case "q":
				if len(a.stack) == 1 {
					return a, tea.Quit
				}
				return a, pop
			case "esc", "backspace":
				// pages get first shot at esc (e.g. clearing a filter)
				p, cmd := a.top().Update(msg)
				a.stack[len(a.stack)-1] = p
				if cmd != nil {
					return a, cmd
				}
				if len(a.stack) > 1 {
					return a, pop
				}
				return a, nil
			case "H":
				if len(a.stack) > 1 {
					for _, p := range a.stack[1:] {
						closePage(p)
					}
					a.stack = a.stack[:1]
					return a, a.forward(refreshMsg{"pop"})
				}
			}
		}
	}
	return a, a.forward(msg)
}

func (a *App) forward(msg tea.Msg) tea.Cmd {
	p, cmd := a.top().Update(msg)
	a.stack[len(a.stack)-1] = p
	return cmd
}

func (a *App) View() string {
	if a.w == 0 {
		return "loading…"
	}
	st := a.ctx.St
	// header: brand › crumbs ……… user
	var crumbs []string
	for _, p := range a.stack {
		if t := p.Title(); t != "" {
			crumbs = append(crumbs, t)
		}
	}
	if len(crumbs) > 3 {
		crumbs = append([]string{"…"}, crumbs[len(crumbs)-3:]...)
	}
	left := st.Brand.Render(a.ctx.F.Cfg.Name) + " " + st.Crumb.Render(strings.Join(crumbs, st.CrumbSep.Render(" › ")))
	who := "anonymous"
	if a.ctx.User != nil {
		who = a.ctx.User.Name
		if a.ctx.User.IsAdmin {
			who += " (admin)"
		}
	}
	right := st.User.Render(who)
	if a.unread > 0 {
		right = st.Label.Render(fmt.Sprintf("● %d ", a.unread)) + right
	}
	gap := a.w - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if gap < 1 {
		left = ansi.Truncate(left, a.w-ansi.StringWidth(right)-3, "…")
		gap = 1
	}
	header := st.Header.Render(left+strings.Repeat(" ", gap)+right) + "\n" + st.Faint.Render(strings.Repeat("─", a.w))

	w, h := a.contentSize()
	var body string
	if a.help {
		body = a.helpView(w, h)
	} else {
		body = a.top().View()
	}
	body = lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).MaxWidth(w).Render(body)

	// footer: toast or key hints
	var foot string
	if a.toast != "" {
		if a.toastE {
			foot = st.Error.Render("✗ " + a.toast)
		} else {
			foot = st.Status.Render("✓ " + a.toast)
		}
	} else {
		keys := append(a.top().Keys(), Key{"?", "help"})
		if len(a.stack) > 1 {
			keys = append(keys, Key{"esc", "back"})
		} else {
			keys = append(keys, Key{"q", "quit"})
		}
		var parts []string
		for _, k := range keys {
			parts = append(parts, st.Key.Render(k.Key)+" "+st.KeyDesc.Render(k.Desc))
		}
		foot = strings.Join(parts, st.Faint.Render(" · "))
	}
	foot = ansi.Truncate(foot, a.w-2, "…")
	footer := st.Faint.Render(strings.Repeat("─", a.w)) + "\n" + st.Footer.Render(foot)
	return header + "\n" + body + "\n" + footer
}

func (a *App) helpView(w, h int) string {
	st := a.ctx.St
	section := func(title string, keys []Key) string {
		var b strings.Builder
		b.WriteString(st.Title.Render(title) + "\n")
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("  %s  %s\n", st.Key.Render(fmt.Sprintf("%-10s", k.Key)), st.KeyDesc.Render(k.Desc)))
		}
		return b.String()
	}
	global := []Key{
		{"↑/↓ j/k", "move"}, {"enter", "open"}, {"tab/⇧tab", "next/prev tab"}, {"1-9, 0", "jump to tab"},
		{"/", "filter list"}, {"esc", "back / clear filter"}, {"H", "home"}, {"r", "refresh"},
		{"ctrl+u/d", "half page up/down"}, {"q", "back / quit"}, {"ctrl+c", "quit"},
	}
	content := section("Global", global) + "\n" + section("This screen", a.top().Keys()) +
		"\n" + st.Faint.Render("press any key to close")
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, st.Help.Render(content))
}

// ---------------------------------------------------------------- tabs helper

type Tabs struct {
	Names  []string
	Active int
}

func (t *Tabs) Update(msg tea.KeyMsg) bool {
	switch s := msg.String(); s {
	case "tab", "right", "l":
		t.Active = (t.Active + 1) % len(t.Names)
	case "shift+tab", "left", "h":
		t.Active = (t.Active + len(t.Names) - 1) % len(t.Names)
	default:
		if len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
			i := int(s[0] - '1')
			if s == "0" {
				i = 9 // the tenth tab
			}
			if i < len(t.Names) {
				t.Active = i
				return true
			}
		}
		return false
	}
	return true
}

func (t *Tabs) View(st *Styles, w int) string {
	render := func(numbers bool) []string {
		var parts []string
		for i, n := range t.Names {
			label := n
			if numbers && i < 10 { // digits 1-9 and 0 select the first ten tabs
				label = fmt.Sprintf("%d %s", (i+1)%10, n)
			}
			if i == t.Active {
				parts = append(parts, st.TabActive.Render(label))
			} else {
				parts = append(parts, st.Tab.Render(label))
			}
		}
		return parts
	}
	parts := render(true)
	if width(parts) > w {
		parts = render(false)
	}
	// still too wide: drop tabs on the left until the active one fits
	// (with room for the "‹" and "…" markers)
	need := func(from int) int {
		n := width(parts[from:t.Active+1]) + 1
		if from > 0 {
			n++
		}
		return n
	}
	from := 0
	for from < t.Active && width(parts) > w && need(from) > w {
		from++
	}
	line := strings.Join(parts[from:], "")
	if from > 0 {
		line = st.Faint.Render("‹") + line
	}
	if ansi.StringWidth(line) > w {
		line = ansi.Truncate(line, w, "…")
	}
	return line
}

func width(parts []string) int {
	n := 0
	for _, p := range parts {
		n += ansi.StringWidth(p)
	}
	return n
}

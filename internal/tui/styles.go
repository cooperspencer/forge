package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/cooperspencer/forge/internal/store"
)

// Styles are built per session from that session's renderer so colour
// profile detection works correctly over SSH.
type Styles struct {
	r  *lipgloss.Renderer
	hl *highlighter

	App, Header, Brand, Crumb, CrumbSep, User                  lipgloss.Style
	Footer, Key, KeyDesc, Status, Error                        lipgloss.Style
	Tab, TabActive, TabBar                                     lipgloss.Style
	Title, Subtle, Faint, Bold, Accent, Box, BoxFocus          lipgloss.Style
	Selected, SelectedBlur, Row, ColHeader                     lipgloss.Style
	Add, Del, Hunk, FileHdr                                    lipgloss.Style
	Label, Badge                                               lipgloss.Style
	Input, InputFocus, Help                                    lipgloss.Style
	StatusOK, StatusFail, StatusRun, StatusWait, StatusNeutral lipgloss.Style
}

var (
	cAccent  = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#A78BFA"}
	cAccent2 = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#67E8F9"}
	cText    = lipgloss.AdaptiveColor{Light: "#1F2937", Dark: "#E5E7EB"}
	cSubtle  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	cFaint   = lipgloss.AdaptiveColor{Light: "#9CA3AF", Dark: "#6B7280"}
	cBorder  = lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#374151"}
	cSelBg   = lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#2E1065"}
	cSelBg2  = lipgloss.AdaptiveColor{Light: "#F3F4F6", Dark: "#1F2937"}
	cGreen   = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	cRed     = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	cYellow  = lipgloss.AdaptiveColor{Light: "#A16207", Dark: "#FACC15"}
	cBlue    = lipgloss.AdaptiveColor{Light: "#1D4ED8", Dark: "#60A5FA"}
)

func NewStyles(r *lipgloss.Renderer) *Styles {
	s := &Styles{r: r, hl: newHighlighter(r)}
	ns := r.NewStyle
	s.App = ns().Foreground(cText)
	s.Header = ns().Padding(0, 1)
	s.Brand = ns().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cAccent).Padding(0, 1)
	s.Crumb = ns().Foreground(cText).Bold(true)
	s.CrumbSep = ns().Foreground(cFaint)
	s.User = ns().Foreground(cSubtle)
	s.Footer = ns().Padding(0, 1).Foreground(cSubtle)
	s.Key = ns().Foreground(cAccent).Bold(true)
	s.KeyDesc = ns().Foreground(cSubtle)
	s.Status = ns().Foreground(cGreen)
	s.Error = ns().Foreground(cRed).Bold(true)
	s.Tab = ns().Foreground(cSubtle).Padding(0, 1)
	s.TabActive = ns().Foreground(cAccent).Bold(true).Padding(0, 1).Underline(true)
	s.TabBar = ns().BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).BorderForeground(cBorder)
	s.Title = ns().Bold(true).Foreground(cText)
	s.Subtle = ns().Foreground(cSubtle)
	s.Faint = ns().Foreground(cFaint)
	s.Bold = ns().Bold(true)
	s.Accent = ns().Foreground(cAccent2)
	s.Box = ns().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	s.BoxFocus = s.Box.BorderForeground(cAccent)
	s.Selected = ns().Background(cSelBg).Bold(true)
	s.SelectedBlur = ns().Background(cSelBg2)
	s.Row = ns()
	s.ColHeader = ns().Foreground(cFaint).Bold(true)
	s.Add = ns().Foreground(cGreen)
	s.Del = ns().Foreground(cRed)
	s.Hunk = ns().Foreground(cBlue)
	s.FileHdr = ns().Bold(true).Foreground(cAccent2)
	s.Label = ns().Foreground(cAccent).Bold(true)
	s.Badge = ns().Padding(0, 1).Foreground(lipgloss.Color("#FFFFFF")).Background(cSubtle)
	s.Input = ns().Border(lipgloss.RoundedBorder()).BorderForeground(cBorder).Padding(0, 1)
	s.InputFocus = s.Input.BorderForeground(cAccent)
	s.Help = ns().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(1, 2)
	s.StatusOK = ns().Foreground(cGreen)
	s.StatusFail = ns().Foreground(cRed)
	s.StatusRun = ns().Foreground(cYellow)
	s.StatusWait = ns().Foreground(cBlue)
	s.StatusNeutral = ns().Foreground(cSubtle)
	return s
}

// StatusIcon renders a coloured glyph + word for CI / issue states.
func (s *Styles) StatusIcon(status string) string {
	switch status {
	case store.StatusSuccess:
		return s.StatusOK.Render("✓")
	case store.StatusFailure, store.StatusError:
		return s.StatusFail.Render("✗")
	case store.StatusRunning:
		return s.StatusRun.Render("●")
	case store.StatusQueued, store.StatusPending, store.StatusBlocked:
		return s.StatusWait.Render("○")
	case store.StatusApproval:
		return s.Label.Render("◇")
	case store.StatusCancelled:
		return s.StatusNeutral.Render("⊘")
	case store.StatusSkipped:
		return s.StatusNeutral.Render("↷")
	case "open":
		return s.StatusOK.Render("●")
	case "closed":
		return s.StatusFail.Render("●")
	case "merged":
		return lipgloss.NewStyle().Foreground(cAccent).Render("◆")
	}
	return s.Faint.Render("·")
}

func (s *Styles) StatusText(status string) string {
	st := s.StatusNeutral
	switch status {
	case store.StatusSuccess, "open":
		st = s.StatusOK
	case store.StatusFailure, store.StatusError, "closed":
		st = s.StatusFail
	case store.StatusRunning:
		st = s.StatusRun
	case store.StatusQueued, store.StatusPending, store.StatusBlocked:
		st = s.StatusWait
	case "merged", store.StatusApproval:
		st = s.Label
	}
	label := status
	if status == store.StatusApproval {
		label = "needs approval"
	}
	return s.StatusIcon(status) + " " + st.Render(label)
}

func ago(t time.Time) string {
	if t.Unix() <= 0 {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

func dur(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1024/1024/1024)
}

// colorDiff styles a unified diff patch line by line.
func (s *Styles) colorDiff(patch string, width int) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		l = expandTabs(l)
		switch {
		case strings.HasPrefix(l, "@@"):
			b.WriteString(s.Hunk.Render(l))
		case strings.HasPrefix(l, "+"):
			b.WriteString(s.Add.Render(l))
		case strings.HasPrefix(l, "-"):
			b.WriteString(s.Del.Render(l))
		default:
			b.WriteString(l)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func trimRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}

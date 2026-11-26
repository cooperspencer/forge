package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/store"
)

// Visual helpers for pipeline runs: status badges, trigger markers, a compact
// per-run job strip and the stage graph on the run page.

var (
	cPink = lipgloss.AdaptiveColor{Light: "#BE185D", Dark: "#F472B6"}
	cTeal = lipgloss.AdaptiveColor{Light: "#0F766E", Dark: "#2DD4BF"}
)

// statusStyle is the foreground colour for a CI status.
func (s *Styles) statusStyle(status string) lipgloss.Style {
	switch status {
	case store.StatusSuccess:
		return s.StatusOK
	case store.StatusFailure, store.StatusError:
		return s.StatusFail
	case store.StatusRunning:
		return s.StatusRun
	case store.StatusQueued, store.StatusPending, store.StatusBlocked:
		return s.StatusWait
	case store.StatusApproval:
		return s.Label
	}
	return s.StatusNeutral
}

var badgeLabel = map[string]string{
	store.StatusSuccess:   "passed",
	store.StatusFailure:   "failed",
	store.StatusError:     "error",
	store.StatusRunning:   "running",
	store.StatusQueued:    "queued",
	store.StatusPending:   "pending",
	store.StatusBlocked:   "pending",
	store.StatusCancelled: "cancelled",
	store.StatusSkipped:   "skipped",
	store.StatusApproval:  "needs approval",
}

// StatusBadge renders a status as a filled pill: " ✓ passed ".
func (s *Styles) StatusBadge(status string) string {
	bg := lipgloss.TerminalColor(cSubtle)
	fg := lipgloss.TerminalColor(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#111827"})
	switch status {
	case store.StatusSuccess:
		bg = cGreen
	case store.StatusFailure, store.StatusError:
		bg = cRed
	case store.StatusRunning:
		bg = cYellow
	case store.StatusQueued, store.StatusPending, store.StatusBlocked:
		bg = cBlue
	case store.StatusApproval:
		bg = cAccent
	}
	label := badgeLabel[status]
	if label == "" {
		label = status
	}
	icon := ansi.Strip(s.StatusIcon(status))
	return s.r.NewStyle().Bold(true).Foreground(fg).Background(bg).Padding(0, 1).Render(icon + " " + label)
}

// Trigger renders the event that started a run with its own marker and colour.
func (s *Styles) Trigger(kind string) string {
	icon, c := "•", lipgloss.TerminalColor(cSubtle)
	switch kind {
	case "push":
		icon, c = "↑", cBlue
	case "tag":
		icon, c = "◈", synType
	case "pull_request":
		icon, c, kind = "⇄", cAccent, "pull request"
	case "manual":
		icon, c = "▶", cPink
	case "schedule":
		icon, c = "◷", cTeal
	}
	return s.r.NewStyle().Foreground(c).Render(icon + " " + kind)
}

// JobName colours a job name by its status: failures stand out, skipped and
// cancelled jobs fade.
func (s *Styles) JobName(j *store.Job, name string) string {
	switch j.Status {
	case store.StatusFailure, store.StatusError:
		return s.StatusFail.Bold(true).Render(name)
	case store.StatusRunning:
		return s.StatusRun.Bold(true).Render(name)
	case store.StatusSkipped, store.StatusCancelled:
		return s.Faint.Render(name)
	case store.StatusQueued, store.StatusBlocked, store.StatusPending, store.StatusApproval:
		return s.Subtle.Render(name)
	}
	return name
}

// Duration colours a duration: running jobs are yellow, finished faint.
func (s *Styles) Duration(d string, status string) string {
	if status == store.StatusRunning {
		return s.StatusRun.Render(d)
	}
	return s.Faint.Render(d)
}

// stages groups jobs by their depth in the needs graph: stage 0 needs
// nothing, stage n needs something from stage n-1. Matrix cells share a key
// and land in the same stage. Order within a stage follows the input.
func stages(jobs []*store.Job) [][]*store.Job {
	byKey := map[string][]*store.Job{}
	for _, j := range jobs {
		byKey[j.Key] = append(byKey[j.Key], j)
	}
	depth := map[string]int{}
	var visit func(key string, seen map[string]bool) int
	visit = func(key string, seen map[string]bool) int {
		if d, ok := depth[key]; ok {
			return d
		}
		if seen[key] || len(byKey[key]) == 0 {
			return 0 // cycle or unknown key: validation prevents both
		}
		seen[key] = true
		d := 0
		for _, n := range byKey[key][0].Needs {
			if nd := visit(n, seen) + 1; nd > d {
				d = nd
			}
		}
		depth[key] = d
		return d
	}
	max := 0
	for _, j := range jobs {
		if d := visit(j.Key, map[string]bool{}); d > max {
			max = d
		}
	}
	out := make([][]*store.Job, max+1)
	for _, j := range jobs {
		d := depth[j.Key]
		out[d] = append(out[d], j)
	}
	return out
}

// JobStrip is one status icon per job, ordered by stage: "✓✓✓✗✓↷".
func (s *Styles) JobStrip(jobs []*store.Job) string {
	var b strings.Builder
	for i, st := range stages(jobs) {
		if i > 0 {
			b.WriteString(s.Faint.Render("›"))
		}
		for _, j := range st {
			b.WriteString(s.StatusIcon(j.Status))
		}
	}
	return b.String()
}

// StageGraph draws the run's stages left to right:
//
//	✓ lint → ✓✓ test → ✗ build → ✓ notify  ↷ deploy
//
// Matrix cells of one job share a name with one icon each. It is cut to width.
func (s *Styles) StageGraph(jobs []*store.Job, width int) string {
	var parts []string
	for _, st := range stages(jobs) {
		var names []string
		groups := map[string][]*store.Job{}
		for _, j := range st {
			if _, ok := groups[j.Key]; !ok {
				names = append(names, j.Key)
			}
			groups[j.Key] = append(groups[j.Key], j)
		}
		var items []string
		for _, k := range names {
			var icons strings.Builder
			for _, j := range groups[k] {
				icons.WriteString(s.StatusIcon(j.Status))
			}
			items = append(items, icons.String()+" "+s.JobName(worst(groups[k]), k))
		}
		parts = append(parts, strings.Join(items, "  "))
	}
	return ansi.Truncate(" "+strings.Join(parts, s.Faint.Render("  →  ")), width, "…")
}

// worst picks the job whose status best describes a group (a failed matrix
// cell colours the whole group).
func worst(js []*store.Job) *store.Job {
	rank := map[string]int{store.StatusFailure: 7, store.StatusError: 7, store.StatusRunning: 6, store.StatusApproval: 5,
		store.StatusQueued: 4, store.StatusPending: 4, store.StatusBlocked: 4, store.StatusCancelled: 3, store.StatusSkipped: 2, store.StatusSuccess: 1}
	out := js[0]
	for _, j := range js[1:] {
		if rank[j.Status] > rank[out.Status] {
			out = j
		}
	}
	return out
}

// Log lines that start like a compiler/tool error or warning get coloured,
// unless the program already coloured them itself.
var (
	logErrRe  = regexp.MustCompile(`^\s*(\[?(error|fatal|panic|ERROR|FATAL|PANIC|Error|Fatal|FAIL|FAILED)\b\]?[:\s!]|--- FAIL)`)
	logWarnRe = regexp.MustCompile(`^\s*\[?(warning|warn|WARNING|WARN|Warning)\b\]?[:\s!]`)
)

func (s *Styles) logLine(l string) string {
	if strings.Contains(l, "\x1b[") {
		return l
	}
	switch {
	case logErrRe.MatchString(l):
		return s.StatusFail.Render(l)
	case logWarnRe.MatchString(l):
		return s.StatusRun.Render(l)
	}
	return l
}

package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/store"
)

// changeLabel is a jj change ID as shown before a commit subject.
func changeLabel(st *Styles, id string) string {
	if id == "" {
		return ""
	}
	return st.Label.Render(gitx.ShortChangeID(id)) + " "
}

// revisionsTab lists the heads a pull request has had; enter shows what
// changed since the previous one.
func (s *issueState) revisionsTab() *Tab {
	st, f, c := s.ctx.St, s.ctx.F, s.ctx.Ctx
	t := &Tab{Name: "Revisions", List: NewList(st, Column{Title: "REV", Width: 4, Right: true}, Column{Title: "HEAD", Width: 8}, Column{Title: "BASE", Width: 8}, Column{Title: "PUSHED BY", Flex: 1}, Column{Title: "WHEN", Width: 10, Right: true})}
	t.List.Empty = "No revisions recorded yet (they are recorded from the next push on)."
	t.Load = func() []Row {
		revs, _ := f.PRRevisions(c, s.ctx.User, s.repo, s.issue)
		var rows []Row
		for k := len(revs) - 1; k >= 0; k-- {
			v := revs[k]
			rows = append(rows, Row{Cols: []string{st.Bold.Render(fmt.Sprint(v.N)), st.Accent.Render(shortSHA(v.HeadSHA)), st.Faint.Render(shortSHA(v.BaseSHA)), st.Subtle.Render(orDash(v.Pusher)), st.Faint.Render(ago(v.CreatedAt))}, Data: v})
		}
		return rows
	}
	t.Keys = []Key{{"enter", "changes since the previous revision"}, {"a", "since the first"}}
	t.Handle = func(k string, row *Row) (tea.Cmd, bool) {
		if row == nil || (k != "enter" && k != "a") {
			return nil, false
		}
		v := row.Data.(store.PRRevision)
		if v.N == 1 {
			return toast("revision 1 is the first push; pick a later one"), true
		}
		from := v.N - 1
		if k == "a" {
			from = 1
		}
		d, err := f.PRInterdiff(c, s.ctx.User, s.repo, s.issue, from, v.N)
		if err != nil {
			return toastErr(err), true
		}
		return push(NewTextPage(s.ctx, fmt.Sprintf("#%d rev %d→%d", s.issue.Number, from, v.N), renderInterdiff(st, d))), true
	}
	return t
}

func renderInterdiff(st *Styles, d *forge.Interdiff) string {
	var b strings.Builder
	b.WriteString(st.Title.Render(fmt.Sprintf("Revision %d → %d", d.From.N, d.To.N)))
	if d.To.Pusher != "" {
		b.WriteString(st.Subtle.Render("  pushed by " + d.To.Pusher + " " + ago(d.To.CreatedAt)))
	}
	b.WriteString("\n")
	if d.Rebased {
		b.WriteString(st.Subtle.Render(fmt.Sprintf("rebased: base %s → %s", shortSHA(d.From.BaseSHA), shortSHA(d.To.BaseSHA))) + "\n")
	}
	b.WriteString("\n" + st.Bold.Render("Changes") + "\n")
	for _, ch := range d.Changes {
		cm := ch.New
		if cm == nil {
			cm = ch.Old
		}
		var status string
		switch ch.Status {
		case forge.ChangeRewritten:
			status = st.StatusWait.Render(fmt.Sprintf("%-9s", ch.Status))
		case forge.ChangeNew:
			status = st.StatusOK.Render(fmt.Sprintf("%-9s", ch.Status))
		case forge.ChangeDropped:
			status = st.StatusFail.Render(fmt.Sprintf("%-9s", ch.Status))
		default:
			status = st.Faint.Render(fmt.Sprintf("%-9s", ch.Status))
		}
		id := st.Accent.Render(cm.Short())
		if ch.ChangeID != "" {
			id = st.Label.Render(gitx.ShortChangeID(ch.ChangeID))
		}
		line := fmt.Sprintf("  %s  %s  %s", id, status, cm.Subject)
		if ch.New != nil && ch.Old != nil && ch.Old.Subject != ch.New.Subject {
			line += st.Faint.Render("  (was: " + ch.Old.Subject + ")")
		}
		b.WriteString(line + "\n")
	}
	if len(d.Changes) == 0 {
		b.WriteString(st.Faint.Render("  no commits") + "\n")
	}
	for _, ch := range d.Changes {
		if ch.Status != forge.ChangeRewritten {
			continue
		}
		b.WriteString("\n" + st.Bold.Render(fmt.Sprintf("What changed in %q", ch.New.Subject)) + "\n")
		if ch.Note != "" {
			b.WriteString(st.Faint.Render(ch.Note) + "\n")
		}
		b.WriteString(renderDiffs(st, ch.Files))
	}
	b.WriteString("\n" + st.Bold.Render("The whole pull request, revision "+fmt.Sprint(d.From.N)+" vs "+fmt.Sprint(d.To.N)) + "\n")
	if d.Note != "" {
		b.WriteString(st.Faint.Render(d.Note) + "\n")
	}
	if len(d.Files) == 0 {
		b.WriteString(st.Faint.Render("no differences in content (only history changed)") + "\n")
	} else {
		b.WriteString(renderDiffs(st, d.Files))
	}
	return b.String()
}

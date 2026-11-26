package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/cooperspencer/forge/internal/store"
)

// RunPage shows a pipeline run: jobs on the left, the selected job's live
// log on the right (stacked on narrow terminals).
type RunPage struct {
	ctx  *Ctx
	repo *store.Repo
	run  *store.Run
	jobs *List
	all  []*store.Job // for the stage graph
	log  viewport.Model

	jobID    int64
	logBuf   strings.Builder
	logCh    <-chan []byte
	unsub    func()
	follow   bool
	focusLog bool
	w, h     int
	side     bool
	gen      int // subscription generation, to drop stale chunks
}

type logChunkMsg struct {
	gen  int
	data []byte
}

func NewRunPage(ctx *Ctx, r *store.Repo, run *store.Run) *RunPage {
	p := &RunPage{ctx: ctx, repo: r, run: run, follow: true, log: viewport.New(80, 20)}
	p.jobs = NewList(ctx.St, Column{Title: "", Width: 1}, Column{Title: "JOB", Flex: 1}, Column{Title: "TIME", Width: 7, Right: true})
	p.jobs.NoHeader = true
	return p
}

func (p *RunPage) Title() string { return fmt.Sprintf("%s #%d", p.run.Pipeline, p.run.Number) }

func (p *RunPage) Keys() []Key {
	if p.run.Status == store.StatusApproval {
		return []Key{{"A", "approve"}, {"c", "cancel"}, {"s", "spec"}, {"tab", "jobs/log"}}
	}
	k := []Key{{"tab", "jobs/log"}, {"f", "follow"}, {"c", "cancel"}, {"R", "rerun"}, {"F", "rerun failed"}, {"a", "artifacts"}, {"s", "spec"}}
	return k
}

func (p *RunPage) Capturing() bool { return p.jobs.Filtering() }

func (p *RunPage) Init() tea.Cmd {
	p.loadJobs()
	return p.subscribe()
}

func (p *RunPage) loadJobs() {
	st, f, c := p.ctx.St, p.ctx.F, p.ctx.Ctx
	if r, err := f.Store.RunByID(c, p.run.ID); err == nil {
		held := p.run.Status == store.StatusApproval
		p.run = r
		if held != (r.Status == store.StatusApproval) && p.w > 0 {
			defer p.SetSize(p.w, p.h) // the approval banner appeared or went away
		}
	}
	jobs, _ := f.Store.Jobs(c, p.run.ID)
	p.all = nil
	var rows []Row
	for _, j := range jobs {
		p.all = append(p.all, j)
		name := st.JobName(j, j.Name)
		if j.Attempt > 1 {
			name += st.Faint.Render(fmt.Sprintf(" (attempt %d)", j.Attempt))
		}
		rows = append(rows, Row{Cols: []string{st.StatusIcon(j.Status), name, st.Duration(dur(j.Duration()), j.Status)}, Filter: j.Name, Data: j})
	}
	if len(jobs) == 0 {
		p.jobs.Empty = "no jobs"
	}
	p.jobs.SetRows(rows)
}

func (p *RunPage) selectedJob() *store.Job {
	if r := p.jobs.Selected(); r != nil {
		return r.Data.(*store.Job)
	}
	return nil
}

// subscribe (re)attaches the log view to the selected job.
func (p *RunPage) subscribe() tea.Cmd {
	j := p.selectedJob()
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
	p.gen++
	p.logBuf.Reset()
	if j == nil {
		p.jobID = 0
		if p.run.Error != "" {
			p.logBuf.WriteString(p.ctx.St.Error.Render("Pipeline definition error:") + "\n\n" + p.run.Error + "\n")
		}
		p.renderLog()
		return nil
	}
	p.jobID = j.ID
	snap, ch, cancel := p.ctx.F.Logs.Subscribe(j.ID)
	p.logBuf.Write(snap)
	p.logCh, p.unsub = ch, cancel
	p.renderLog()
	if p.follow {
		p.log.GotoBottom()
	}
	return p.waitLog(p.gen, ch)
}

func (p *RunPage) waitLog(gen int, ch <-chan []byte) tea.Cmd {
	return func() tea.Msg {
		b, ok := <-ch
		if !ok {
			return nil
		}
		return logChunkMsg{gen, b}
	}
}

func (p *RunPage) renderLog() {
	raw := p.logBuf.String()
	if raw == "" {
		j := p.selectedJob()
		switch {
		case j == nil:
		case j.Status == store.StatusQueued:
			raw = p.ctx.St.Faint.Render("Waiting for a runner to pick up this job…")
			if j.Labels != nil {
				raw += p.ctx.St.Faint.Render(" (requires labels: " + strings.Join(j.Labels, ", ") + ")")
			}
		case j.Status == store.StatusBlocked:
			raw = p.ctx.St.Faint.Render("Waiting for: " + strings.Join(j.Needs, ", "))
		case j.Status == store.StatusSkipped:
			raw = p.ctx.St.Faint.Render("Skipped because a dependency did not succeed.")
		default:
			raw = p.ctx.St.Faint.Render("No output.")
		}
	}
	p.log.SetContent(renderTerminal(raw, p.log.Width, p.ctx.St))
}

// renderTerminal emulates carriage returns (progress bars), colours error
// and warning lines (with st) and wraps lines.
func renderTerminal(s string, w int, st *Styles) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.Contains(l, "\r") {
			parts := strings.Split(strings.TrimRight(l, "\r"), "\r")
			l = parts[len(parts)-1]
		}
		l = expandTabs(l)
		if st != nil {
			l = st.logLine(l)
		}
		if w > 0 && ansi.StringWidth(l) > w {
			l = ansi.Hardwrap(l, w, true)
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func (p *RunPage) SetSize(w, h int) {
	p.w, p.h = w, h
	p.side = w >= 100
	body := h - 4 // header, stage graph, rule, status
	if p.run.Status == store.StatusApproval {
		body-- // approval banner
	}
	if p.side {
		jw := 36
		p.jobs.SetSize(jw, body)
		p.log.Width, p.log.Height = w-jw-3, body
	} else {
		jh := p.jobs.Len() + 1
		if jh > body/3 {
			jh = body / 3
		}
		if jh < 2 {
			jh = 2
		}
		p.jobs.SetSize(w, jh)
		p.log.Width, p.log.Height = w, body-jh-1
	}
	p.renderLog()
	if p.follow {
		p.log.GotoBottom()
	}
}

func (p *RunPage) Update(msg tea.Msg) (Page, tea.Cmd) {
	f, c := p.ctx.F, p.ctx.Ctx
	switch msg := msg.(type) {
	case logChunkMsg:
		if msg.gen != p.gen {
			return p, nil
		}
		p.logBuf.Write(msg.data)
		p.renderLog()
		if p.follow {
			p.log.GotoBottom()
		}
		return p, p.waitLog(p.gen, p.logCh)
	case refreshMsg:
		if msg.topic == fmt.Sprintf("run:%d", p.run.ID) || msg.topic == "pop" || msg.topic == "local" {
			prev := p.jobID
			p.loadJobs()
			if j := p.selectedJob(); j != nil && j.ID != prev {
				return p, p.subscribe()
			}
			// a re-run truncates logs: resubscribe if the file shrank
			if j := p.selectedJob(); j != nil {
				if b, _ := f.Logs.Read(j.ID); len(b) < p.logBuf.Len() {
					return p, p.subscribe()
				}
			}
			p.renderLog()
		}
		return p, nil
	case tickMsg:
		if !store.IsTerminal(p.run.Status) {
			p.loadJobs()
		}
		return p, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "enter":
			p.focusLog = !p.focusLog
			p.jobs.Focus(!p.focusLog)
			return p, nil
		case "f":
			p.follow = !p.follow
			if p.follow {
				p.log.GotoBottom()
				return p, toast("following log")
			}
			return p, toast("follow off")
		case "c":
			return p, do(func() error { return f.CancelRun(c, p.ctx.User, p.repo, p.run) }, "cancellation requested", refreshCmd)
		case "A":
			return p, approveRun(p.ctx, p.repo, p.run)
		case "R", "F":
			failed := msg.String() == "F"
			return p, do(func() error { return f.RerunRun(c, p.ctx.User, p.repo, p.run, failed) }, "re-running", refreshCmd)
		case "a":
			return p, push(p.artifactsPage())
		case "s":
			if j := p.selectedJob(); j != nil {
				return p, push(NewTextPage(p.ctx, "spec", j.Spec))
			}
			return p, nil
		case "g":
			if p.focusLog {
				p.follow = false
				p.log.GotoTop()
				return p, nil
			}
		case "G":
			if p.focusLog {
				p.follow = true
				p.log.GotoBottom()
				return p, nil
			}
		}
		if p.focusLog {
			var cmd tea.Cmd
			p.log, cmd = p.log.Update(msg)
			if !p.log.AtBottom() {
				p.follow = false
			}
			return p, cmd
		}
		prev := p.jobID
		if ok, cmd := p.jobs.Update(msg); ok {
			if j := p.selectedJob(); j != nil && j.ID != prev {
				return p, tea.Batch(cmd, p.subscribe())
			}
			return p, cmd
		}
	}
	return p, nil
}

func (p *RunPage) artifactsPage() Page {
	st := p.ctx.St
	t := &Tab{Name: "Artifacts", List: NewList(st, Column{Title: "NAME", Flex: 1}, Column{Title: "JOB", Width: 8}, Column{Title: "SIZE", Width: 10, Right: true}, Column{Title: "URL", Flex: 2})}
	t.List.Empty = "This run produced no artifacts."
	t.Load = func() []Row {
		as, _ := p.ctx.F.Store.RunArtifacts(p.ctx.Ctx, p.run.ID)
		var rows []Row
		for _, a := range as {
			url := fmt.Sprintf("%s/api/v1/repos/%s/artifacts/%d", p.ctx.F.Cfg.HTTP.PublicURL, p.repo.FullName(), a.ID)
			rows = append(rows, Row{Cols: []string{st.Bold.Render(a.Name), fmt.Sprint(a.JobID), humanSize(a.Size), st.Accent.Render(url)}})
		}
		return rows
	}
	t.Header = func() string {
		return st.Faint.Render(" Download with: ") + st.Accent.Render(p.ctx.F.Cfg.SSHCommand()+fmt.Sprintf(" ci artifact %s <id> > file", p.repo.FullName()))
	}
	return NewTabPage(p.ctx, "artifacts", []*Tab{t})
}

func (p *RunPage) View() string {
	st := p.ctx.St
	r := p.run
	ref := trimRef(r.Ref)
	if r.PRNumber > 0 {
		ref = fmt.Sprintf("PR #%d", r.PRNumber)
	}
	sep := st.Faint.Render(" · ")
	head := " " + st.StatusBadge(r.Status) + "  " + st.Title.Render(fmt.Sprintf("%s #%d", r.Pipeline, r.Number)) + "  " +
		st.Trigger(r.Trigger) + sep + st.Accent.Render(ref) + sep + st.Faint.Render(shortSHA(r.SHA)) + sep + st.Duration(dur(r.Duration()), r.Status)
	if r.Actor != "" {
		head += sep + st.Subtle.Render("by "+r.Actor)
	}
	head = ansi.Truncate(head, p.w, "…")
	follow := st.Faint.Render("follow off")
	if p.follow {
		follow = st.Label.Render("● following")
	}
	jobTitle := "log"
	if j := p.selectedJob(); j != nil {
		jobTitle = st.StatusIcon(j.Status) + " " + st.JobName(j, j.Name)
		if j.Runner != "" {
			jobTitle += st.Faint.Render(" on " + j.Runner)
		}
	}
	logHead := st.Bold.Render(jobTitle) + "  " + follow
	if r.Status == store.StatusApproval {
		who := "an outside contributor's fork"
		if r.Actor != "" {
			who = r.Actor + "'s fork"
		}
		banner := " " + st.Label.Render("◇ Runs code from "+who) + st.Subtle.Render(" · review the PR first · runs without secrets")
		head += "\n" + ansi.Truncate(banner, p.w, "…")
	}
	head += "\n" + st.StageGraph(p.all, p.w)
	head += "\n" + st.Faint.Render(strings.Repeat("─", p.w))
	jobsBox, logBox := p.jobs.View(), p.log.View()
	if p.side {
		sep := strings.TrimSuffix(strings.Repeat(" "+st.Faint.Render("│")+" \n", p.log.Height+1), "\n")
		jcol := lipgloss.JoinVertical(lipgloss.Left, st.Subtle.Render(" Jobs"), jobsBox)
		lcol := lipgloss.JoinVertical(lipgloss.Left, logHead, logBox)
		return head + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, jcol, sep, lcol)
	}
	return head + "\n" + jobsBox + "\n" + logHead + "\n" + logBox
}

func (p *RunPage) Close() {
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
}

// approveRun asks for confirmation, then releases a run that waits for
// approval.
func approveRun(ctx *Ctx, repo *store.Repo, run *store.Run) tea.Cmd {
	if run.Status != store.StatusApproval {
		return toast(fmt.Sprintf("run #%d is not waiting for approval", run.Number))
	}
	who := "an outside contributor"
	if run.Actor != "" {
		who = run.Actor
	}
	q := fmt.Sprintf("Run code by %s (%s) on this repository's runners?", who, shortSHA(run.SHA))
	return push(NewConfirm(ctx, q, func() tea.Cmd {
		return do(func() error { return ctx.F.ApproveRun(ctx.Ctx, ctx.User, repo, run) }, fmt.Sprintf("approved #%d", run.Number), refreshCmd)
	}))
}

package sshd

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type ciCmds struct {
	Runs     ciRunsCmd     `cmd:"" help:"List pipeline runs."`
	View     ciViewCmd     `cmd:"" help:"Show the jobs of a run."`
	Logs     ciLogsCmd     `cmd:"" help:"Print (or follow) job logs."`
	Trigger  ciTriggerCmd  `cmd:"" help:"Start a pipeline manually."`
	Approve  ciApproveCmd  `cmd:"" help:"Run a held pull request run from a fork."`
	Cancel   ciCancelCmd   `cmd:"" help:"Cancel a run."`
	Rerun    ciRerunCmd    `cmd:"" help:"Re-run a run."`
	Artifact ciArtifactCmd `cmd:"" help:"Write an artifact to stdout."`
	Lint     ciLintCmd     `cmd:"" help:"Validate pipeline files."`
}

type secretCmds struct {
	List secretListCmd `cmd:"" help:"List CI secret names (of a repository, or a user's/organization's inherited ones)."`
	Set  secretSetCmd  `cmd:"" help:"Set a secret (the value is read from stdin if omitted)."`
	Rm   secretRmCmd   `cmd:"" help:"Delete a secret."`
}

type runArgs struct {
	Repo   string `arg:"" help:"owner/name"`
	Number string `arg:"" name:"run" help:"Run number."`
}

type ciRunsCmd struct {
	N    int    `short:"n" name:"limit" default:"20" help:"Number of runs."`
	Repo string `arg:"" help:"owner/name"`
}

func (x *ciRunsCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	runs, err := c.F.Store.Runs(c.Ctx, r.ID, x.N)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "RUN\tSTATUS\tPIPELINE\tTRIGGER\tREF\tSHA\tDURATION\tCREATED")
	for _, run := range runs {
		fmt.Fprintf(w, "#%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", run.Number, run.Status, run.Pipeline, run.Trigger, run.Ref, run.SHA[:8], run.Duration(), ts(run.CreatedAt))
	}
	return w.Flush()
}

type ciViewCmd struct {
	runArgs `embed:""`
}

func (x *ciViewCmd) Run(c *CLI) error {
	_, run, err := c.run(x.Repo, x.Number)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "%s #%d: %s (%s %s @ %s)\n", run.Pipeline, run.Number, run.Status, run.Trigger, run.Ref, run.SHA[:8])
	if run.Error != "" {
		fmt.Fprintf(c.Out, "error: %s\n", run.Error)
	}
	jobs, err := c.F.Store.Jobs(c.Ctx, run.ID)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "ID\tJOB\tSTATUS\tRUNNER\tDURATION")
	for _, j := range jobs {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", j.ID, j.Name, j.Status, j.Runner, j.Duration())
	}
	return w.Flush()
}

type ciLogsCmd struct {
	Follow  bool `short:"f" help:"Follow the logs until the jobs finish."`
	runArgs `embed:""`
	Job     string `arg:"" optional:"" help:"Only this job (key, name or id)."`
}

func (x *ciLogsCmd) Run(c *CLI) error {
	_, run, err := c.run(x.Repo, x.Number)
	if err != nil {
		return err
	}
	jobs, err := c.F.Store.Jobs(c.Ctx, run.ID)
	if err != nil {
		return err
	}
	var sel []*store.Job
	for _, j := range jobs {
		if x.Job == "" || x.Job == j.Key || x.Job == j.Name || x.Job == strconv.FormatInt(j.ID, 10) {
			sel = append(sel, j)
		}
	}
	if len(sel) == 0 {
		return fmt.Errorf("no matching job")
	}
	for _, j := range sel {
		if len(sel) > 1 {
			fmt.Fprintf(c.Out, "\n===== %s =====\n", j.Name)
		}
		snap, ch, cancel := c.F.Logs.Subscribe(j.ID)
		c.Out.Write(snap)
		if !x.Follow {
			cancel()
			continue
		}
		tick := time.NewTicker(time.Second)
	loop:
		for {
			select {
			case b := <-ch:
				c.Out.Write(b)
			case <-tick.C:
				cur, err := c.F.Store.JobByID(c.Ctx, j.ID)
				if err != nil || (store.IsTerminal(cur.Status) && len(ch) == 0) {
					break loop
				}
			case <-c.Ctx.Done():
				break loop
			}
		}
		tick.Stop()
		cancel()
	}
	return nil
}

type ciTriggerCmd struct {
	Pipeline string `short:"p" help:"Pipeline name."`
	Repo     string `arg:"" help:"owner/name"`
	Ref      string `arg:"" optional:"" help:"Branch, tag or commit (default: the default branch)."`
}

func (x *ciTriggerCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessWrite)
	if err != nil {
		return err
	}
	runs, err := c.F.TriggerManual(c.Ctx, c.User, r, x.Ref, x.Pipeline)
	if err != nil {
		return err
	}
	for _, run := range runs {
		fmt.Fprintf(c.Out, "started %s #%d\n", run.Pipeline, run.Number)
	}
	return nil
}

type ciApproveCmd struct {
	runArgs `embed:""`
}

func (x *ciApproveCmd) Run(c *CLI) error {
	r, run, err := c.run(x.Repo, x.Number)
	if err != nil {
		return err
	}
	if err := c.F.ApproveRun(c.Ctx, c.User, r, run); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "approved %s #%d (%s); it will run without secrets\n", run.Pipeline, run.Number, run.SHA[:min(8, len(run.SHA))])
	return nil
}

type ciCancelCmd struct {
	runArgs `embed:""`
}

func (x *ciCancelCmd) Run(c *CLI) error {
	r, run, err := c.run(x.Repo, x.Number)
	if err != nil {
		return err
	}
	return c.F.CancelRun(c.Ctx, c.User, r, run)
}

type ciRerunCmd struct {
	Failed  bool `help:"Only the failed jobs."`
	runArgs `embed:""`
}

func (x *ciRerunCmd) Run(c *CLI) error {
	r, run, err := c.run(x.Repo, x.Number)
	if err != nil {
		return err
	}
	return c.F.RerunRun(c.Ctx, c.User, r, run, x.Failed)
}

type ciArtifactCmd struct {
	Repo string `arg:"" help:"owner/name"`
	ID   string `arg:"" name:"artifact-id"`
}

func (x *ciArtifactCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	id, err := atoi64(x.ID, "artifact id")
	if err != nil {
		return err
	}
	a, err := c.F.Store.ArtifactByID(c.Ctx, id)
	if err != nil {
		return err
	}
	j, err := c.F.Store.JobByID(c.Ctx, a.JobID)
	if err != nil {
		return err
	}
	run, err := c.F.Store.RunByID(c.Ctx, j.RunID)
	if err != nil || run.RepoID != r.ID {
		return store.ErrNotFound
	}
	f, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(c.Out, f)
	return err
}

type ciLintCmd struct {
	Repo string `arg:"" help:"owner/name"`
	Ref  string `arg:"" optional:""`
}

func (x *ciLintCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessRead)
	if err != nil {
		return err
	}
	ref := x.Ref
	if ref == "" {
		ref = r.DefaultBranch
	}
	g := c.F.Git(r)
	sha, err := g.ResolveRef(c.Ctx, ref)
	if err != nil {
		return err
	}
	pipes, errs := loadPipelines(c.Ctx, g, sha)
	for f, p := range pipes {
		jobs := p.Expand("")
		fmt.Fprintf(c.Out, "ok    %s (%d job(s) after matrix expansion)\n", f, len(jobs))
	}
	for f, e := range errs {
		fmt.Fprintf(c.Out, "error %s: %v\n", f, e)
	}
	if len(pipes)+len(errs) == 0 {
		fmt.Fprintln(c.Out, "no pipeline files found (.forge.yml or .forge/pipelines/*.yml)")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d invalid pipeline file(s)", len(errs))
	}
	return nil
}

// ---------------------------------------------------------------- secrets

// secretTarget resolves "owner/name" to a repository or a bare "owner" to a
// user or organization whose secrets all its repositories inherit.
func (c *CLI) secretTarget(arg string) (*store.Repo, *store.User, error) {
	if strings.Contains(arg, "/") {
		r, err := c.repo(arg, store.AccessAdmin)
		return r, nil, err
	}
	o, err := c.F.Store.UserByName(c.Ctx, arg)
	if err != nil {
		return nil, nil, fmt.Errorf("no user or organization %q", arg)
	}
	return nil, o, nil
}

type secretListCmd struct {
	Target string `arg:"" placeholder:"owner/name|owner" help:"A repository, or a user or organization."`
}

func (x *secretListCmd) Run(c *CLI) error {
	r, o, err := c.secretTarget(x.Target)
	if err != nil {
		return err
	}
	var ss []forge.SecretInfo
	if r != nil {
		ss, err = c.F.RepoSecrets(c.Ctx, c.User, r)
	} else {
		ss, err = c.F.OwnerSecrets(c.Ctx, c.User, o)
	}
	if err != nil {
		return err
	}
	for _, s := range ss {
		from := ""
		if s.Owner != "" {
			from = "\tinherited from " + s.Owner
			if s.Overridden {
				from += " (overridden by repo)"
			}
		}
		fmt.Fprintf(c.Out, "%s\t(updated %s)%s\n", s.Name, ts(s.UpdatedAt), from)
	}
	return nil
}

type secretSetCmd struct {
	Target string   `arg:"" placeholder:"owner/name|owner" help:"A repository, or a user or organization."`
	Name   string   `arg:""`
	Value  []string `arg:"" optional:"" help:"The value (default: read from stdin, which keeps it out of shell history)."`
}

func (x *secretSetCmd) Run(c *CLI) error {
	r, o, err := c.secretTarget(x.Target)
	if err != nil {
		return err
	}
	var val string
	if len(x.Value) > 0 {
		val = strings.Join(x.Value, " ")
	} else {
		b, err := io.ReadAll(io.LimitReader(c.In, 64<<10+1))
		if err != nil {
			return err
		}
		val = strings.TrimRight(string(b), "\r\n")
	}
	if r != nil {
		err = c.F.SetRepoSecret(c.Ctx, c.User, r, x.Name, val)
	} else {
		err = c.F.SetOwnerSecret(c.Ctx, c.User, o, x.Name, val)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "secret", strings.ToUpper(x.Name), "saved")
	return nil
}

type secretRmCmd struct {
	Target string `arg:"" placeholder:"owner/name|owner" help:"A repository, or a user or organization."`
	Name   string `arg:""`
}

func (x *secretRmCmd) Run(c *CLI) error {
	r, o, err := c.secretTarget(x.Target)
	if err != nil {
		return err
	}
	if r != nil {
		return c.F.DeleteRepoSecret(c.Ctx, c.User, r, x.Name)
	}
	return c.F.DeleteOwnerSecret(c.Ctx, c.User, o, x.Name)
}

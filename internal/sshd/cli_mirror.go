package sshd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type mirrorCmds struct {
	List   mirrorListCmd   `cmd:"" help:"List a repository's mirrors (admin)."`
	Create mirrorCreateCmd `cmd:"" help:"Create a repository that mirrors a remote."`
	Push   mirrorPushCmd   `cmd:"" help:"Push the repository to a remote after every push (admin)."`
	Sync   mirrorSyncCmd   `cmd:"" help:"Sync now (all mirrors, or one)."`
	Key    mirrorIDCmd     `cmd:"" help:"Print the deploy key of an ssh mirror."`
	Rm     mirrorRmCmd     `cmd:"" help:"Remove a mirror (a pull mirror becomes a normal repository)."`
}

func (c *CLI) token(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	t, err := c.bodyArg("-")
	return strings.TrimSpace(t), err
}

type mirrorListCmd struct {
	Repo string `arg:"" help:"owner/name"`
}

func (x *mirrorListCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	ms, err := c.F.Mirrors(c.Ctx, c.User, r)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Fprintln(c.Out, "no mirrors")
		return nil
	}
	w := c.table()
	for _, m := range ms {
		state := "synced " + ts(m.LastSync)
		if m.LastError != "" {
			state = "FAILED: " + m.LastError
		} else if m.LastSync.IsZero() {
			state = "not synced yet"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\tevery %s\t%s\n", m.ID, m.Direction, m.URL, forge.FormatInterval(m.Interval), state)
	}
	return w.Flush()
}

type mirrorCreateCmd struct {
	User     string `help:"Username for a private https remote."`
	Token    string `help:"Password or token (- reads stdin)."`
	Interval string `default:"8h" help:"How often to sync (10m … 30d)."`
	Private  bool   `help:"Private repository."`
	CI       bool   `name:"ci" help:"Run pipelines for mirrored commits."`
	URL      string `arg:"" name:"remote-url"`
	Name     string `arg:"" optional:"" placeholder:"[owner/]name" help:"Repository to create (default: from the URL)."`
}

func (x *mirrorCreateCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	iv, err := forge.ParseInterval(x.Interval)
	if err != nil {
		return err
	}
	pass, err := c.token(x.Token)
	if err != nil {
		return err
	}
	owner, name := "", x.Name
	if o, n, ok := strings.Cut(name, "/"); ok {
		owner, name = o, n
	}
	r, err := c.F.CreatePullMirror(c.Ctx, c.User, owner, name, "", x.Private, forge.MirrorOptions{URL: x.URL, Username: x.User, Password: pass, Interval: iv, RunCI: x.CI})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "created %s, mirroring %s (first sync running; see: mirror list %s)\n", r.FullName(), x.URL, r.FullName())
	return nil
}

type mirrorPushCmd struct {
	User     string `help:"Username for an https remote."`
	Token    string `help:"Password or token (- reads stdin)."`
	Interval string `default:"0" help:"Also sync this often (0: only after pushes)."`
	Repo     string `arg:"" help:"owner/name"`
	URL      string `arg:"" name:"remote-url"`
}

func (x *mirrorPushCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	iv, err := forge.ParseInterval(x.Interval)
	if err != nil {
		return err
	}
	pass, err := c.token(x.Token)
	if err != nil {
		return err
	}
	m, err := c.F.AddPushMirror(c.Ctx, c.User, r, forge.MirrorOptions{URL: x.URL, Username: x.User, Password: pass, Interval: iv})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "push mirror %d added: %s\n", m.ID, m.URL)
	if m.SSHPub != "" {
		fmt.Fprintf(c.Out, "add this deploy key (with write access) to the remote, then run: mirror sync %s %d\n\n%s\n", r.FullName(), m.ID, m.SSHPub)
	}
	return nil
}

func (c *CLI) mirrorArg(full, id string) (*store.Repo, int64, error) {
	r, err := c.repo(full, store.AccessAdmin)
	if err != nil {
		return nil, 0, err
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid mirror id %q (see mirror list)", id)
	}
	return r, n, nil
}

type mirrorSyncCmd struct {
	Repo string `arg:"" help:"owner/name"`
	ID   string `arg:"" optional:"" name:"id" help:"Only this mirror (see mirror list)."`
}

func (x *mirrorSyncCmd) Run(c *CLI) error {
	r, err := c.repo(x.Repo, store.AccessAdmin)
	if err != nil {
		return err
	}
	ms, err := c.F.Mirrors(c.Ctx, c.User, r)
	if err != nil {
		return err
	}
	n := 0
	for _, m := range ms {
		if x.ID != "" && strconv.FormatInt(m.ID, 10) != x.ID {
			continue
		}
		if err := c.F.SyncMirror(c.Ctx, m.ID); err != nil {
			fmt.Fprintf(c.Out, "%s %s: FAILED: %v\n", m.Direction, m.URL, err)
		} else {
			fmt.Fprintf(c.Out, "%s %s: synced\n", m.Direction, m.URL)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("no such mirror")
	}
	return nil
}

type mirrorRmCmd struct {
	Repo string `arg:"" help:"owner/name"`
	ID   string `arg:"" name:"id" help:"See mirror list."`
}

func (x *mirrorRmCmd) Run(c *CLI) error {
	r, id, err := c.mirrorArg(x.Repo, x.ID)
	if err != nil {
		return err
	}
	return c.F.RemoveMirror(c.Ctx, c.User, r, id)
}

type mirrorIDCmd struct {
	Repo string `arg:"" help:"owner/name"`
	ID   string `arg:"" name:"id" help:"See mirror list."`
}

func (x *mirrorIDCmd) Run(c *CLI) error {
	r, id, err := c.mirrorArg(x.Repo, x.ID)
	if err != nil {
		return err
	}
	ms, err := c.F.Mirrors(c.Ctx, c.User, r)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.ID == id {
			if m.SSHPub == "" {
				return fmt.Errorf("mirror %d uses https; deploy keys are for ssh remotes", id)
			}
			fmt.Fprintln(c.Out, m.SSHPub)
			return nil
		}
	}
	return fmt.Errorf("no such mirror")
}

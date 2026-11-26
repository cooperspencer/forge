package sshd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type runnerCmds struct {
	List    runnerListCmd   `cmd:"" help:"List runners (admin)."`
	Create  runnerCreateCmd `cmd:"" help:"Register a runner and print its token (admin)."`
	Rm      runnerNameCmd   `cmd:"" help:"Delete a runner (admin)."`
	Disable runnerToggleCmd `cmd:"" help:"Stop assigning jobs to a runner (admin)."`
	Enable  runnerToggleCmd `cmd:"" help:"Resume assigning jobs to a runner (admin)."`
}

type userCmds struct {
	List   userListCmd   `cmd:"" help:"List users (admin)."`
	Create userCreateCmd `cmd:"" help:"Create a user (admin)."`
	Delete userDeleteCmd `cmd:"" help:"Delete a user and their repositories (admin)."`
	Admin  userAdminCmd  `cmd:"" help:"Grant or revoke site admin (admin)."`
	Addkey userAddKeyCmd `cmd:"" help:"Add an SSH key to a user (admin)."`
	Rename userRenameCmd `cmd:"" help:"Rename yourself, or a user (admin) or organization (its owners)."`
}

type keyCmds struct {
	List keyListCmd `cmd:"" help:"List your SSH keys."`
	Add  keyAddCmd  `cmd:"" help:"Add an SSH key."`
	Rm   keyRmCmd   `cmd:"" help:"Remove an SSH key."`
}

type tokenCmds struct {
	List   tokenListCmd   `cmd:"" help:"List your API tokens."`
	Create tokenCreateCmd `cmd:"" help:"Create an API token (printed once)."`
	Rm     tokenRmCmd     `cmd:"" help:"Revoke an API token."`
}

type orgCmds struct {
	Create  orgCreateCmd  `cmd:"" help:"Create an organization."`
	Members orgMembersCmd `cmd:"" help:"List members."`
	Add     orgAddCmd     `cmd:"" help:"Add or update a member."`
	Remove  orgRemoveCmd  `cmd:"" help:"Remove a member."`
}

type notifCmds struct {
	List notifListCmd `cmd:"" help:"Show your inbox."`
	Read notifReadCmd `cmd:"" help:"Mark everything as read."`
}

// ---------------------------------------------------------------- runners

type runnerListCmd struct{}

func (runnerListCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	rs, err := c.F.Store.Runners(c.Ctx)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "NAME\tSTATE\tLABELS\tVERSION\tLAST SEEN")
	for _, r := range rs {
		state := "offline"
		if r.Online() {
			state = "online"
		}
		if !r.Enabled {
			state += " (disabled)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, state, strings.Join(r.Labels, ","), r.Version, ts(r.LastSeen))
	}
	return w.Flush()
}

type runnerCreateCmd struct {
	Name   string `arg:""`
	Labels string `arg:"" optional:"" help:"Comma-separated extra labels."`
}

func (x *runnerCreateCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	var labels []string
	if x.Labels != "" {
		labels = strings.Split(x.Labels, ",")
	}
	tok, err := c.F.Store.CreateRunner(c.Ctx, x.Name, labels)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "runner %s registered. Start it with:\n\n  forge runner --server %s --token %s\n", x.Name, c.F.Cfg.HTTP.PublicURL, tok)
	return nil
}

func (c *CLI) runnerByName(name string) (*store.Runner, error) {
	rs, err := c.F.Store.Runners(c.Ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, store.ErrNotFound
}

type runnerNameCmd struct {
	Name string `arg:""`
}

func (x *runnerNameCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	r, err := c.runnerByName(x.Name)
	if err != nil {
		return err
	}
	return c.F.Store.DeleteRunner(c.Ctx, r.ID)
}

type runnerToggleCmd struct {
	on   bool
	Name string `arg:""`
}

func (x *runnerToggleCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	r, err := c.runnerByName(x.Name)
	if err != nil {
		return err
	}
	return c.F.Store.SetRunnerEnabled(c.Ctx, r.ID, x.on)
}

// ---------------------------------------------------------------- users

type userListCmd struct{}

func (userListCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	us, err := c.F.Store.ListUsers(c.Ctx)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "NAME\tTYPE\tADMIN\tCREATED")
	for _, u := range us {
		typ := "user"
		if u.IsOrg {
			typ = "org"
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", u.Name, typ, u.IsAdmin, ts(u.CreatedAt))
	}
	return w.Flush()
}

type userCreateCmd struct {
	Admin bool     `help:"Make the user a site admin."`
	Name  string   `arg:""`
	Key   []string `arg:"" optional:"" name:"ssh-key" help:"Their public key (ssh-ed25519 AAAA… comment)."`
}

func (x *userCreateCmd) Run(c *CLI) error {
	u, err := c.F.AdminCreateUser(c.Ctx, c.User, x.Name, strings.Join(x.Key, " "), x.Admin)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "created user", u.Name)
	return nil
}

type userDeleteCmd struct {
	Name string `arg:""`
}

func (x *userDeleteCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.Name)
	if err != nil {
		return err
	}
	if u.ID == c.User.ID {
		return fmt.Errorf("you cannot delete yourself")
	}
	repos, _ := c.F.Store.VisibleRepos(c.Ctx, c.User)
	for _, r := range repos {
		if r.OwnerID == u.ID {
			if err := c.F.DeleteRepo(c.Ctx, c.User, r); err != nil {
				return err
			}
		}
	}
	return c.F.Store.DeleteUser(c.Ctx, u.ID)
}

type userAdminCmd struct {
	Name  string `arg:""`
	Admin string `arg:"" placeholder:"true|false"`
}

func (x *userAdminCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.Name)
	if err != nil {
		return err
	}
	if u.IsAdmin, err = strconv.ParseBool(x.Admin); err != nil {
		return fmt.Errorf("say true or false, not %q", x.Admin)
	}
	return c.F.Store.UpdateUser(c.Ctx, u)
}

type userAddKeyCmd struct {
	Name string   `arg:""`
	Key  []string `arg:"" name:"ssh-key"`
}

func (x *userAddKeyCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.Name)
	if err != nil {
		return err
	}
	return c.F.AddKey(c.Ctx, u, strings.Join(x.Key, " "))
}

type userRenameCmd struct {
	First  string `arg:"" placeholder:"[name] new-name" help:"The new name, or the user/organization to rename followed by its new name."`
	Second string `arg:"" optional:"" hidden:""`
}

func (x *userRenameCmd) Run(c *CLI) error {
	target, newName := c.User, x.First
	if x.Second != "" {
		u, err := c.F.Store.UserByName(c.Ctx, x.First)
		if err != nil {
			return err
		}
		target, newName = u, x.Second
	}
	if target == nil {
		return c.needUser()
	}
	old := target.Name
	if err := c.F.RenameUser(c.Ctx, c.User, target, newName); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "renamed %s to %s\n", old, target.Name)
	if old != target.Name {
		fmt.Fprintf(c.Out, "update your remotes, e.g.: git remote set-url origin %s\n", c.F.Cfg.HTTP.PublicURL+"/"+target.Name+"/REPO.git")
	}
	return nil
}

// ---------------------------------------------------------------- keys

type keyListCmd struct{}

func (keyListCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	ks, err := c.F.Store.SSHKeys(c.Ctx, c.User.ID)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "ID\tFINGERPRINT\tTITLE\tADDED")
	for _, k := range ks {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", k.ID, k.Fingerprint, k.Title, ts(k.CreatedAt))
	}
	return w.Flush()
}

type keyAddCmd struct {
	Key []string `arg:"" name:"ssh-key" help:"ssh-ed25519 AAAA… comment"`
}

func (x *keyAddCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	if err := c.F.AddKey(c.Ctx, c.User, strings.Join(x.Key, " ")); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "key added")
	return nil
}

type keyRmCmd struct {
	ID string `arg:"" name:"id" help:"See key list."`
}

func (x *keyRmCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	id, err := atoi64(x.ID, "key id")
	if err != nil {
		return err
	}
	ks, _ := c.F.Store.SSHKeys(c.Ctx, c.User.ID)
	if len(ks) == 1 {
		return fmt.Errorf("refusing to remove your last key")
	}
	return c.F.Store.DeleteSSHKey(c.Ctx, c.User.ID, id)
}

// ---------------------------------------------------------------- tokens

type tokenListCmd struct{}

func (tokenListCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	toks, err := c.F.Store.AccessTokens(c.Ctx, c.User.ID)
	if err != nil {
		return err
	}
	w := c.table()
	fmt.Fprintln(w, "ID\tNAME\tSCOPES\tREPOS\tEXPIRES\tCREATED\tLAST USED")
	for _, t := range toks {
		scopes, repos := "full access", "all"
		if !t.Grant.Full {
			scopes = strings.ReplaceAll(t.Grant.String(), " ", ",")
		}
		if len(t.Grant.Repos) > 0 {
			repos = strings.Join(t.Grant.Repos, ",")
		}
		exp := "never"
		if !t.ExpiresAt.IsZero() {
			exp = ts(t.ExpiresAt)
			if t.Expired() {
				exp += " (expired)"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, scopes, repos, exp, ts(t.CreatedAt), ts(t.LastUsed))
	}
	return w.Flush()
}

type tokenCreateCmd struct {
	Scopes  string `default:"*" placeholder:"code:write,issues:read,…" help:"What the token may do: code, issues, ci, packages (each :read or :write), admin, site-admin; * is full access."`
	Repos   string `placeholder:"owner/name,owner/*" help:"Limit the token to these repositories (default: all)."`
	Expires string `default:"never" placeholder:"30d|1y|2027-03-31|never" help:"Lifetime, date, or never."`
	Name    string `arg:""`
}

func (x *tokenCreateCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	g, err := forge.GrantFrom(x.Scopes, x.Repos)
	if err != nil {
		return err
	}
	exp, err := forge.ParseExpiry(x.Expires, time.Now())
	if err != nil {
		return err
	}
	tok, err := c.F.CreateToken(c.Ctx, c.User, forge.TokenSpec{Name: x.Name, Grant: g, Expires: exp})
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, tok)
	return nil
}

type tokenRmCmd struct {
	ID string `arg:"" name:"id" help:"See token list."`
}

func (x *tokenRmCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	id, err := strconv.ParseInt(x.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid token id %q", x.ID)
	}
	return c.F.Store.DeleteAccessToken(c.Ctx, c.User.ID, id)
}

// ---------------------------------------------------------------- orgs

type orgCreateCmd struct {
	Name string `arg:""`
}

func (x *orgCreateCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	if _, err := c.F.CreateOrg(c.Ctx, c.User, x.Name); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "created organization", x.Name)
	return nil
}

func (c *CLI) org(name string) (*store.User, error) {
	o, err := c.F.Store.UserByName(c.Ctx, name)
	if err != nil || !o.IsOrg {
		return nil, fmt.Errorf("unknown organization %q", name)
	}
	return o, nil
}

type orgMembersCmd struct {
	Org string `arg:""`
}

func (x *orgMembersCmd) Run(c *CLI) error {
	o, err := c.org(x.Org)
	if err != nil {
		return err
	}
	ms, err := c.F.Store.OrgMembers(c.Ctx, o.ID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		fmt.Fprintf(c.Out, "%s\t%s\n", m.User.Name, m.Role)
	}
	return nil
}

type orgAddCmd struct {
	Org  string `arg:""`
	User string `arg:""`
	Role string `arg:"" optional:"" default:"member" enum:"owner,member" help:"owner or member."`
}

func (x *orgAddCmd) Run(c *CLI) error {
	o, err := c.org(x.Org)
	if err != nil {
		return err
	}
	if !c.F.CanManageOrg(c.Ctx, c.User, o) {
		return forge.ErrForbidden
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.User)
	if err != nil || u.IsOrg {
		return fmt.Errorf("unknown user %q", x.User)
	}
	return c.F.Store.SetOrgMember(c.Ctx, o.ID, u.ID, x.Role)
}

type orgRemoveCmd struct {
	Org  string `arg:""`
	User string `arg:""`
}

func (x *orgRemoveCmd) Run(c *CLI) error {
	o, err := c.org(x.Org)
	if err != nil {
		return err
	}
	if !c.F.CanManageOrg(c.Ctx, c.User, o) {
		return forge.ErrForbidden
	}
	u, err := c.F.Store.UserByName(c.Ctx, x.User)
	if err != nil {
		return err
	}
	return c.F.Store.RemoveOrgMember(c.Ctx, o.ID, u.ID)
}

// ---------------------------------------------------------------- notifications

type notifListCmd struct{}

func (notifListCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	evs, err := c.F.Store.Notifications(c.Ctx, c.User.ID, 50)
	if err != nil {
		return err
	}
	w := c.table()
	for _, e := range evs {
		mark := " "
		if !e.Read {
			mark = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, ts(e.CreatedAt), e.Repo, e.Summary)
	}
	return w.Flush()
}

type notifReadCmd struct{}

func (notifReadCmd) Run(c *CLI) error {
	if err := c.needUser(); err != nil {
		return err
	}
	return c.F.Store.MarkNotificationsRead(c.Ctx, c.User.ID)
}

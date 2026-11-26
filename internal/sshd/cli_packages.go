package sshd

import (
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/forge"
	"github.com/cooperspencer/forge/internal/store"
)

type packageCmds struct {
	List packageListCmd `cmd:"" help:"List packages you can see."`
	Show packageShowCmd `cmd:"" help:"Show tags (images) or versions and files."`
	Rm   packageRmCmd   `cmd:"" help:"Delete a tag, version or file, or the whole package."`
	GC   packageGCCmd   `cmd:"" name:"gc" help:"Remove unreferenced package data now (admin)."`
}

func (c *CLI) pkgActor() forge.PkgActor { return forge.PkgActor{User: c.User} }

type packageListCmd struct {
	Owner string `arg:"" optional:"" help:"Only this user's or organization's packages."`
}

func (x *packageListCmd) Run(c *CLI) error {
	var owner *store.User
	if x.Owner != "" {
		var err error
		if owner, err = c.F.Store.UserByName(c.Ctx, x.Owner); err != nil {
			return fmt.Errorf("no user or organization %q", x.Owner)
		}
	}
	pkgs, err := c.F.VisiblePackages(c.Ctx, c.pkgActor(), owner)
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		fmt.Fprintf(c.Out, "%-10s %-40s updated %s\n", p.Type, p.FullName(), ts(p.UpdatedAt))
	}
	return nil
}

type packageShowCmd struct {
	Type    string `enum:"container,generic," default:"" help:"container or generic (when both exist)."`
	Package string `arg:"" placeholder:"owner/name"`
}

func (x *packageShowCmd) Run(c *CLI) error {
	p, _, _, err := c.F.ResolvePackage(c.Ctx, c.pkgActor(), x.Package, x.Type)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "%s (%s)\n", p.FullName(), p.Type)
	switch p.Type {
	case store.PkgContainer:
		ref := c.F.ImageRef(p)
		tags, err := c.F.Store.Tags(c.Ctx, p.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "pull: docker pull %s:<tag>\n\n", ref)
		for _, t := range tags {
			fmt.Fprintf(c.Out, "%-24s %s  %8s  %-22s %s\n", t.Name, t.Digest[:19], human(c.F.ImageSize(c.Ctx, p, t.Digest)),
				c.F.ImageKind(c.Ctx, p, t.Digest), ts(t.UpdatedAt))
		}
	default:
		files, err := c.F.Store.Files(c.Ctx, p.ID)
		if err != nil {
			return err
		}
		fmt.Fprintln(c.Out)
		for _, f := range files {
			fmt.Fprintf(c.Out, "%-12s %-32s %8s  sha256:%s  %s\n", f.Version, f.Filename, human(f.Size), f.Digest[7:19], ts(f.CreatedAt))
		}
		if len(files) > 0 {
			fmt.Fprintf(c.Out, "\ndownload: curl -LO %s\n", c.F.GenericURL(p, files[0].Version, files[0].Filename))
		}
	}
	return nil
}

type packageRmCmd struct {
	Type    string `enum:"container,generic," default:"" help:"container or generic (when both exist)."`
	Package string `arg:"" placeholder:"owner/name"`
	Version string `arg:"" optional:"" placeholder:"tag|digest|version" help:"Only this tag, digest or version."`
	File    string `arg:"" optional:"" help:"Only this file of a generic package version."`
}

func (x *packageRmCmd) Run(c *CLI) error {
	p, owner, _, err := c.F.ResolvePackage(c.Ctx, c.pkgActor(), x.Package, x.Type)
	if err != nil {
		return err
	}
	if x.Version == "" {
		if err := c.F.DeletePackage(c.Ctx, c.pkgActor(), owner, p); err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "deleted package %s (%s); space is reclaimed by the next gc\n", p.FullName(), p.Type)
		return nil
	}
	if err := c.F.DeletePackageVersion(c.Ctx, c.pkgActor(), owner, p, x.Version, x.File); err != nil {
		return err
	}
	fmt.Fprintln(c.Out, "deleted", strings.TrimSuffix(x.Version+"/"+x.File, "/"))
	return nil
}

type packageGCCmd struct{}

func (packageGCCmd) Run(c *CLI) error {
	if err := c.admin(); err != nil {
		return err
	}
	st, err := c.F.GCPackages(c.Ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "removed %d blob(s) (%s), %d stale link(s), %d abandoned upload(s), %d empty package(s)\n",
		st.Blobs, human(st.Bytes), st.Links, st.Uploads, st.Packages)
	return nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cooperspencer/forge/internal/gitx"
	"github.com/cooperspencer/forge/internal/protect"
	"github.com/cooperspencer/forge/internal/store"
)

// refTarget resolves where a new branch or tag points: a branch, tag or
// commit, by default the default branch.
func (f *Forge) refTarget(ctx context.Context, r *store.Repo, from string) (string, error) {
	g := f.Git(r)
	if from == "" {
		from = r.DefaultBranch
	}
	for _, cand := range []string{"refs/heads/" + from, "refs/tags/" + from, from} {
		if sha, err := g.ResolveRef(ctx, cand); err == nil {
			return sha, nil
		}
	}
	return "", fmt.Errorf("unknown branch, tag or commit %q", from)
}

func (f *Forge) newRef(ctx context.Context, actor *store.User, r *store.Repo, full string) error {
	if actor == nil || f.Store.AccessLevel(ctx, actor, r) < store.AccessWrite {
		return ErrForbidden
	}
	if r.Mirror {
		return f.ReadOnlyError(ctx, r)
	}
	name := strings.TrimPrefix(strings.TrimPrefix(full, "refs/heads/"), "refs/tags/")
	if name == "" || strings.HasPrefix(name, "-") || gitx.CheckRefName(ctx, full) != nil {
		return fmt.Errorf("%q is not a valid name (no spaces, ~ ^ : ? * [ \\, .., or leading -)", name)
	}
	if _, err := f.Git(r).ResolveRef(ctx, full); err == nil {
		return fmt.Errorf("%s already exists", name)
	}
	return nil
}

// CreateBranch creates branch name at from (a branch, tag or commit; default
// the default branch). Branch protection applies as for a push.
func (f *Forge) CreateBranch(ctx context.Context, actor *store.User, r *store.Repo, name, from string) error {
	full := "refs/heads/" + name
	if err := f.newRef(ctx, actor, r, full); err != nil {
		return err
	}
	sha, err := f.refTarget(ctx, r, from)
	if err != nil {
		return err
	}
	rules, _ := f.Store.Protections(ctx, r.ID)
	if err := protect.CheckPush(rules, f.protectAccess(ctx, actor, r), protect.Update{Old: protect.ZeroSHA, New: sha, Ref: full}, nil); err != nil {
		return err
	}
	g := f.Git(r)
	before, _ := g.RefMap(ctx)
	if err := g.UpdateRef(ctx, full, sha, gitx.ZeroSHA); err != nil {
		return errors.New(name + " already exists")
	}
	after, _ := g.RefMap(ctx)
	f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	return nil
}

// CreateTag creates tag name at target (default: the default branch); with
// a message it is an annotated tag.
func (f *Forge) CreateTag(ctx context.Context, actor *store.User, r *store.Repo, name, target, message string) error {
	full := "refs/tags/" + name
	if err := f.newRef(ctx, actor, r, full); err != nil {
		return err
	}
	sha, err := f.refTarget(ctx, r, target)
	if err != nil {
		return err
	}
	g := f.Git(r)
	before, _ := g.RefMap(ctx)
	if err := g.CreateTag(ctx, name, sha, strings.TrimSpace(message), actor.Name); err != nil {
		return err
	}
	after, _ := g.RefMap(ctx)
	f.PostReceive(ctx, actor, r, DiffRefs(before, after))
	return nil
}

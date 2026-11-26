package forge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cooperspencer/forge/internal/store"
)

// TokenSpec is an access token to create.
type TokenSpec struct {
	Name    string
	Grant   *store.Grant // nil: full access
	Expires time.Time    // zero: never
}

// CreateToken creates an access token for actor and returns it (shown once).
func (f *Forge) CreateToken(ctx context.Context, actor *store.User, spec TokenSpec) (string, error) {
	if actor == nil || actor.Grant != nil {
		return "", ErrForbidden // tokens don't mint tokens
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" || len(spec.Name) > 64 {
		return "", errors.New("a token needs a name (up to 64 characters)")
	}
	if g := spec.Grant; g != nil {
		if !g.Full && len(g.Scopes) == 0 {
			return "", errors.New("a token needs at least one scope")
		}
		if g.Scopes[store.ScopeSite] != store.AccessNone && !actor.IsAdmin {
			return "", errors.New("only site admins can create site-admin tokens")
		}
		if g.Full {
			g.Repos = nil // full access means all repositories
		}
	}
	if !spec.Expires.IsZero() && !spec.Expires.After(time.Now()) {
		return "", errors.New("the expiry date is in the past")
	}
	return f.Store.CreateToken(ctx, actor.ID, store.NewToken{Name: spec.Name, Grant: spec.Grant, Expires: spec.Expires})
}

// ParseExpiry parses a token lifetime: "never" (or ""), a duration like
// "12h", "30d", "6w", "1y", or a date "2027-03-31" (end of that day, UTC).
func ParseExpiry(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(" days", "d", " day", "d", " weeks", "w", " week", "w", " years", "y", " year", "y", " hours", "h").Replace(s)
	switch s {
	case "", "never", "0":
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Add(24*time.Hour - time.Second), nil
	}
	if len(s) > 1 {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err == nil && n > 0 {
			switch s[len(s)-1] {
			case 'h':
				return now.Add(time.Duration(n) * time.Hour), nil
			case 'd':
				return now.AddDate(0, 0, n), nil
			case 'w':
				return now.AddDate(0, 0, 7*n), nil
			case 'y':
				return now.AddDate(n, 0, 0), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("expiry %q: use never, a lifetime like 30d, 12h, 6w, 1y, or a date like 2027-03-31", s)
}

// GrantFrom builds a grant from scopes ("code:write issues:read admin" or
// "*") and a repository list ("owner/name owner/*"; empty = all).
func GrantFrom(scopes, repos string) (*store.Grant, error) {
	g, err := store.ParseScopes(scopes)
	if err != nil {
		return nil, err
	}
	if g.Repos, err = store.ParseRepos(repos); err != nil {
		return nil, err
	}
	if g.Full && len(g.Repos) > 0 {
		return nil, errors.New("full access covers all repositories; pick scopes to limit a token to some")
	}
	return g, nil
}

// ExpiryText describes when a token expires, for listings.
func ExpiryText(t store.AccessToken, now time.Time) string {
	switch {
	case t.ExpiresAt.IsZero():
		return "never expires"
	case t.Expired():
		return "expired " + t.ExpiresAt.UTC().Format("2006-01-02")
	}
	d := t.ExpiresAt.Sub(now)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("expires in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("expires in %dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("expires in %dd", int(d.Hours()/24))
	}
	return "expires " + t.ExpiresAt.UTC().Format("2006-01-02")
}

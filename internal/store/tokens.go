package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Access token scopes. A token can do what its user can do, limited to its
// scopes and (optionally) to some repositories. Tokens never grant more than
// anonymous access to repositories outside their list.
const (
	ScopeCode     = "code"       // git (clone, push, LFS), files, commits, branches, tags, releases, wiki, code search
	ScopeIssues   = "issues"     // issues, pull requests, comments, reviews, the board, notifications
	ScopeCI       = "ci"         // pipeline runs, logs and artifacts (write: trigger, cancel, approve, rerun)
	ScopePackages = "packages"   // the container and generic package registries
	ScopeAdmin    = "admin"      // settings: create/delete/rename repositories, collaborators, secrets, mirrors, branch protection
	ScopeSite     = "site-admin" // site administration (site admins only): every repository, runners
)

// LeveledScopes are granted as read or write; ScopeAdmin and ScopeSite are on/off.
var LeveledScopes = []string{ScopeCode, ScopeIssues, ScopeCI, ScopePackages}

var ScopeNames = append(append([]string{}, LeveledScopes...), ScopeAdmin, ScopeSite)

// ErrScope is returned when a token lacks a scope (HTTP 403).
var ErrScope = errors.New("insufficient token scope")

// ErrTokenExpired: the token exists but expired.
var ErrTokenExpired = errors.New("token expired")

// Grant is what a token may do. A nil *Grant means "no limits".
type Grant struct {
	Full   bool              // everything the user can do, now and in future versions
	Scopes map[string]Access // leveled scopes → read/write; admin, site-admin → AccessAdmin
	Repos  []string          // "owner/name" or "owner/*"; empty = all repositories
}

// FullGrant is the "super token": no limits beyond the user's own.
func FullGrant() *Grant { return &Grant{Full: true} }

// ParseScopes parses "code:write issues:read admin" (commas work too), or
// "*" for full access.
func ParseScopes(s string) (*Grant, error) {
	g := &Grant{Scopes: map[string]Access{}}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		name, lvl, hasLvl := strings.Cut(strings.ToLower(f), ":")
		switch {
		case name == "*" || name == "all" || name == "full":
			g.Full = true
		case name == ScopeAdmin || name == ScopeSite:
			if hasLvl {
				return nil, fmt.Errorf("scope %s takes no level", name)
			}
			g.Scopes[name] = AccessAdmin
		case isLeveled(name):
			a := AccessWrite
			if hasLvl {
				switch lvl {
				case "read":
					a = AccessRead
				case "write":
				case "none":
					continue
				default:
					return nil, fmt.Errorf("scope %s: level must be read or write", name)
				}
			}
			if a > g.Scopes[name] {
				g.Scopes[name] = a
			}
		default:
			return nil, fmt.Errorf("unknown scope %q (scopes: %s, or * for full access)", f, strings.Join(ScopeNames, ", "))
		}
	}
	if g.Full {
		g.Scopes = nil
	} else if len(g.Scopes) == 0 {
		return nil, errors.New("a token needs at least one scope")
	}
	return g, nil
}

func isLeveled(s string) bool {
	for _, l := range LeveledScopes {
		if s == l {
			return true
		}
	}
	return false
}

// ParseRepos parses a list of "owner/name" or "owner/*" (commas or spaces).
func ParseRepos(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		f = strings.TrimSuffix(f, ".git")
		o, n, ok := strings.Cut(f, "/")
		if !ok || o == "" || n == "" || strings.ContainsAny(n, "/") || strings.Contains(o, "*") || (strings.Contains(n, "*") && n != "*") {
			return nil, fmt.Errorf("repository %q: use owner/name or owner/*", f)
		}
		out = append(out, f)
	}
	return out, nil
}

// String is the stored form of the scopes ("*" for full access).
func (g *Grant) String() string {
	if g == nil || g.Full {
		return "*"
	}
	var out []string
	for _, n := range ScopeNames {
		switch a := g.Scopes[n]; {
		case a == AccessNone:
		case isLeveled(n):
			out = append(out, n+":"+a.String())
		default:
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// Summary is a one-line description for listings.
func (g *Grant) Summary() string {
	s := "full access"
	if g != nil && !g.Full {
		s = g.String()
	}
	if g != nil && len(g.Repos) > 0 {
		s += " · " + strings.Join(g.Repos, ", ")
	}
	return s
}

// Allows reports whether the grant includes scope at level need (admin and
// site-admin: any level).
func (g *Grant) Allows(scope string, need Access) bool {
	if g == nil || g.Full {
		return true
	}
	a := g.Scopes[scope]
	if !isLeveled(scope) {
		return a != AccessNone
	}
	return a >= need
}

// Require is Allows as an error that names the missing scope.
func (g *Grant) Require(scope string, need Access) error {
	if g.Allows(scope, need) {
		return nil
	}
	if isLeveled(scope) {
		scope += ":" + need.String()
	}
	return fmt.Errorf("%w: this token lacks the %s scope", ErrScope, scope)
}

// Covers reports whether the grant applies to repository owner/name.
func (g *Grant) Covers(owner, name string) bool {
	if g == nil || g.Full || len(g.Repos) == 0 {
		return true
	}
	for _, p := range g.Repos {
		po, pn, _ := strings.Cut(p, "/")
		if strings.EqualFold(po, owner) && (pn == "*" || strings.EqualFold(pn, name)) {
			return true
		}
	}
	return false
}

// CoversOwner reports whether the grant applies to everything of owner
// (packages without a repository, owner secrets): all repositories or "owner/*".
func (g *Grant) CoversOwner(owner string) bool {
	if g == nil || g.Full || len(g.Repos) == 0 {
		return true
	}
	for _, p := range g.Repos {
		if po, pn, _ := strings.Cut(p, "/"); strings.EqualFold(po, owner) && pn == "*" {
			return true
		}
	}
	return false
}

// Cap is the highest repository access the scopes can use.
func (g *Grant) Cap() Access {
	if g == nil || g.Full || g.Scopes[ScopeAdmin] != AccessNone || g.Scopes[ScopeSite] != AccessNone {
		return AccessAdmin
	}
	c := AccessRead
	for _, n := range LeveledScopes {
		if g.Scopes[n] > c {
			c = g.Scopes[n]
		}
	}
	return c
}

// SiteAdmin: the user is a site admin and (for tokens) may act as one.
func (u *User) SiteAdmin() bool {
	return u != nil && u.IsAdmin && u.Grant.Allows(ScopeSite, AccessAdmin)
}

// ------------------------------------------------------------------ storage

type AccessToken struct {
	ID        int64
	Name      string
	Grant     *Grant
	CreatedAt time.Time
	LastUsed  time.Time
	ExpiresAt time.Time // zero: never
}

func (t AccessToken) Expired() bool { return !t.ExpiresAt.IsZero() && !time.Now().Before(t.ExpiresAt) }

// NewToken describes a token to create.
type NewToken struct {
	Name    string
	Grant   *Grant    // nil: full access
	Expires time.Time // zero: never
}

// CreateToken returns the plaintext token; only its hash is stored.
func (s *Store) CreateToken(ctx context.Context, userID int64, nt NewToken) (string, error) {
	tok, hash := RandomToken("fgp_")
	var exp int64
	if !nt.Expires.IsZero() {
		exp = nt.Expires.Unix()
	}
	var repos string
	if nt.Grant != nil {
		repos = strings.Join(nt.Grant.Repos, " ")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO access_tokens(user_id,name,token_hash,created_at,scopes,repos,expires_at) VALUES (?,?,?,?,?,?,?)`,
		userID, nt.Name, hash, now(), nt.Grant.String(), repos, exp)
	return tok, err
}

// CreateAccessToken creates a full-access token that never expires.
func (s *Store) CreateAccessToken(ctx context.Context, userID int64, name string) (string, error) {
	return s.CreateToken(ctx, userID, NewToken{Name: name})
}

func loadGrant(scopes, repos string) *Grant {
	g, err := ParseScopes(scopes)
	if err != nil {
		// unreadable scopes (written by a newer version?) grant nothing
		g = &Grant{Scopes: map[string]Access{}}
	}
	g.Repos, _ = ParseRepos(repos)
	if g.Repos == nil && repos != "" {
		g.Repos = []string{"-/-"} // unparsable list: match nothing rather than everything
	}
	return g
}

func (s *Store) AccessTokens(ctx context.Context, userID int64) ([]AccessToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_at,last_used,scopes,repos,expires_at FROM access_tokens WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessToken
	for rows.Next() {
		var t AccessToken
		var c, l, e int64
		var scopes, repos string
		if err := rows.Scan(&t.ID, &t.Name, &c, &l, &scopes, &repos, &e); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsed, t.Grant = time.Unix(c, 0), time.Unix(l, 0), loadGrant(scopes, repos)
		if e > 0 {
			t.ExpiresAt = time.Unix(e, 0)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAccessToken(ctx context.Context, userID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM access_tokens WHERE id=? AND user_id=?`, id, userID)
	return err
}

// UserByToken returns the token's user with the token's Grant attached.
func (s *Store) UserByToken(ctx context.Context, token string) (*User, error) {
	h := HashToken(token)
	var id, exp int64
	var scopes, repos string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, scopes, repos, expires_at FROM access_tokens WHERE token_hash=?`, h).Scan(&id, &scopes, &repos, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if exp > 0 && time.Now().Unix() >= exp {
		return nil, fmt.Errorf("%w on %s", ErrTokenExpired, time.Unix(exp, 0).UTC().Format("2006-01-02"))
	}
	u, err := s.UserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	u.Grant = loadGrant(scopes, repos)
	s.db.ExecContext(ctx, `UPDATE access_tokens SET last_used=? WHERE token_hash=?`, now(), h)
	return u, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repo struct {
	ID            int64
	OwnerID       int64
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	ForkOf        int64
	ForkOfName    string // "owner/name" of the parent, if any
	LFS           bool   // accepts Git LFS uploads
	Mirror        bool   // read-only pull mirror of a remote
	OwnerIsOrg    bool   // the owner is an organization
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (r *Repo) FullName() string { return r.Owner + "/" + r.Name }

const repoSelect = `SELECT r.id, r.owner_id, u.name, r.name, r.description, r.private, r.default_branch,
	COALESCE(r.fork_of,0), COALESCE(pu.name || '/' || p.name, ''), r.created_at, r.updated_at, r.lfs,
	EXISTS(SELECT 1 FROM mirrors mi WHERE mi.repo_id=r.id AND mi.direction='pull'), u.is_org
	FROM repos r JOIN users u ON u.id=r.owner_id
	LEFT JOIN repos p ON p.id=r.fork_of LEFT JOIN users pu ON pu.id=p.owner_id`

func scanRepo(sc interface{ Scan(...any) error }) (*Repo, error) {
	var r Repo
	var c, u int64
	if err := sc.Scan(&r.ID, &r.OwnerID, &r.Owner, &r.Name, &r.Description, &r.Private, &r.DefaultBranch, &r.ForkOf, &r.ForkOfName, &c, &u, &r.LFS, &r.Mirror, &r.OwnerIsOrg); err != nil {
		return nil, notFound(err)
	}
	r.CreatedAt, r.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return &r, nil
}

func (s *Store) CreateRepo(ctx context.Context, ownerID int64, name, desc string, private bool, forkOf int64, defaultBranch string) (*Repo, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	var fork any
	if forkOf != 0 {
		fork = forkOf
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO repos(owner_id,name,description,private,default_branch,fork_of,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		ownerID, name, desc, b2i(private), defaultBranch, fork, t, t)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("repository %q already exists", name)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.RepoByID(ctx, id)
}

func (s *Store) RepoByID(ctx context.Context, id int64) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, repoSelect+` WHERE r.id=?`, id))
}

func (s *Store) RepoByName(ctx context.Context, owner, name string) (*Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, repoSelect+` WHERE u.name=? AND r.name=?`, owner, strings.TrimSuffix(name, ".git")))
}

func (s *Store) UpdateRepo(ctx context.Context, r *Repo) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET description=?, private=?, default_branch=?, lfs=?, updated_at=? WHERE id=?`,
		r.Description, b2i(r.Private), r.DefaultBranch, b2i(r.LFS), now(), r.ID)
	return err
}

func (s *Store) TouchRepo(ctx context.Context, id int64) {
	s.db.ExecContext(ctx, `UPDATE repos SET updated_at=? WHERE id=?`, now(), id)
}

func (s *Store) RenameRepo(ctx context.Context, id int64, name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET name=?, updated_at=? WHERE id=?`, name, now(), id)
	return err
}

func (s *Store) DeleteRepo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id=?`, id)
	return err
}

// VisibleRepos lists repositories the user can read (nil user = anonymous).
func (s *Store) VisibleRepos(ctx context.Context, u *User) ([]*Repo, error) {
	q := repoSelect + ` WHERE r.private=0`
	args := []any{}
	if u != nil {
		if u.SiteAdmin() {
			q = repoSelect + ` WHERE 1=1`
		} else {
			q += ` OR r.owner_id=? OR EXISTS (SELECT 1 FROM collaborators c WHERE c.repo_id=r.id AND c.user_id=?)
				OR EXISTS (SELECT 1 FROM org_members m WHERE m.org_id=r.owner_id AND m.user_id=?)`
			args = append(args, u.ID, u.ID, u.ID)
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY r.updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		if !r.Private || u == nil || u.Grant.Covers(r.Owner, r.Name) {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ access

type Access int

const (
	AccessNone Access = iota
	AccessRead
	AccessWrite
	AccessAdmin
)

func (a Access) String() string {
	return [...]string{"none", "read", "write", "admin"}[a]
}

func ParseAccess(s string) (Access, error) {
	switch s {
	case "read":
		return AccessRead, nil
	case "write":
		return AccessWrite, nil
	case "admin":
		return AccessAdmin, nil
	}
	return AccessNone, fmt.Errorf("role must be read, write or admin")
}

// AccessLevel resolves the effective permission of u (nil = anonymous) on r.
func (s *Store) AccessLevel(ctx context.Context, u *User, r *Repo) Access {
	if u != nil && u.Grant != nil {
		// tokens: nothing beyond anonymous outside their repositories, and
		// nothing beyond what their scopes can use inside
		if !u.Grant.Covers(r.Owner, r.Name) {
			return s.accessLevel(ctx, nil, r)
		}
		return min(s.accessLevel(ctx, u, r), u.Grant.Cap())
	}
	return s.accessLevel(ctx, u, r)
}

func (s *Store) accessLevel(ctx context.Context, u *User, r *Repo) Access {
	base := AccessNone
	if !r.Private {
		base = AccessRead
	}
	if u == nil {
		return base
	}
	if u.SiteAdmin() || u.ID == r.OwnerID {
		return AccessAdmin
	}
	if role, _ := s.OrgRole(ctx, r.OwnerID, u.ID); role == "owner" {
		return AccessAdmin
	} else if role == "member" && base < AccessWrite {
		base = AccessWrite
	}
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM collaborators WHERE repo_id=? AND user_id=?`, r.ID, u.ID).Scan(&role)
	if err == nil {
		if a, err := ParseAccess(role); err == nil && a > base {
			base = a
		}
	}
	return base
}

type Collaborator struct {
	User *User
	Role string
}

func (s *Store) SetCollaborator(ctx context.Context, repoID, userID int64, role string) error {
	if _, err := ParseAccess(role); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO collaborators(repo_id,user_id,role) VALUES (?,?,?)
		ON CONFLICT(repo_id,user_id) DO UPDATE SET role=excluded.role`, repoID, userID, role)
	return err
}

func (s *Store) RemoveCollaborator(ctx context.Context, repoID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM collaborators WHERE repo_id=? AND user_id=?`, repoID, userID)
	return err
}

func (s *Store) Collaborators(ctx context.Context, repoID int64) ([]Collaborator, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, u.full_name, u.email, u.is_admin, u.is_org, u.created_at, c.role
		FROM collaborators c JOIN users u ON u.id=c.user_id WHERE c.repo_id=? ORDER BY u.name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collaborator
	for rows.Next() {
		var u User
		var ts int64
		var c Collaborator
		if err := rows.Scan(&u.ID, &u.Name, &u.FullName, &u.Email, &u.IsAdmin, &u.IsOrg, &ts, &c.Role); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(ts, 0)
		c.User = &u
		out = append(out, c)
	}
	return out, rows.Err()
}

// CanCreateIn reports whether u may create repos under owner (self or an org they own/belong to).
func (s *Store) CanCreateIn(ctx context.Context, u *User, owner *User) bool {
	if u == nil {
		return false
	}
	if !u.Grant.CoversOwner(owner.Name) {
		return false
	}
	if u.SiteAdmin() || u.ID == owner.ID {
		return true
	}
	if owner.IsOrg {
		role, _ := s.OrgRole(ctx, owner.ID, u.ID)
		return role != ""
	}
	return false
}

// ------------------------------------------------------------------ watches

func (s *Store) Watch(ctx context.Context, userID, repoID int64, on bool) error {
	var err error
	if on {
		_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO watches(user_id,repo_id) VALUES (?,?)`, userID, repoID)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM watches WHERE user_id=? AND repo_id=?`, userID, repoID)
	}
	return err
}

func (s *Store) IsWatching(ctx context.Context, userID, repoID int64) bool {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM watches WHERE user_id=? AND repo_id=?`, userID, repoID).Scan(&x)
	return !errors.Is(err, sql.ErrNoRows) && err == nil
}

// Package store persists forge metadata in SQLite. Git objects themselves
// live in plain bare repositories on disk; this package never touches them.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serialises writers anyway; a single connection avoids SQLITE_BUSY
	// storms and keeps transactions simple.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var cur int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&cur); err != nil {
		return err
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func now() int64 { return time.Now().Unix() }

// RandomToken returns a URL-safe random secret and its storage hash.
func RandomToken(prefix string) (token, hash string) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = prefix + hex.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ------------------------------------------------------------------ users

type User struct {
	ID        int64
	Name      string
	FullName  string
	Email     string
	IsAdmin   bool
	IsOrg     bool
	CreatedAt time.Time
	// Grant limits what this user may do when they authenticated with an
	// access token; nil (SSH, the TUI) means the user's own permissions.
	Grant *Grant
}

const userCols = `id, name, full_name, email, is_admin, is_org, created_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var ts int64
	if err := sc.Scan(&u.ID, &u.Name, &u.FullName, &u.Email, &u.IsAdmin, &u.IsOrg, &ts); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = time.Unix(ts, 0)
	return &u, nil
}

// reservedNames collide with top-level HTTP paths (/v2/ is the container
// registry, /api/ the REST API and package endpoints).
var reservedNames = map[string]bool{"v2": true, "api": true, "healthz": true}

// ValidUserName checks a user or organization name.
func ValidUserName(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, name string, admin, org bool) (*User, error) {
	if err := ValidUserName(name); err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO users(name, is_admin, is_org, created_at) VALUES (?,?,?,?)`, name, b2i(admin), b2i(org), now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("name %q is taken", name)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE name=?`, name))
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY is_org, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET full_name=?, email=?, is_admin=? WHERE id=?`, u.FullName, u.Email, b2i(u.IsAdmin), u.ID)
	return err
}

// RenameUser changes a user's or organization's name. Everything else refers
// to users by ID; the caller moves the repository directory.
func (s *Store) RenameUser(ctx context.Context, id int64, name string) error {
	if err := ValidUserName(name); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE users SET name=? WHERE id=?`, name, id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("name %q is taken", name)
	}
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	return err
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_org=0`).Scan(&n)
	return n, err
}

// ValidName restricts user, org and repo names to a filesystem- and URL-safe set.
func ValidName(n string) error {
	if n == "" || len(n) > 64 {
		return fmt.Errorf("name must be 1-64 characters")
	}
	if n[0] == '.' || n[0] == '-' || strings.HasSuffix(n, ".git") || strings.HasSuffix(strings.ToLower(n), ".wiki") {
		return fmt.Errorf("invalid name %q", n)
	}
	for _, r := range n {
		ok := r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("invalid character %q in name", r)
		}
	}
	return nil
}

// ------------------------------------------------------------------ orgs

type OrgMember struct {
	User *User
	Role string
}

func (s *Store) SetOrgMember(ctx context.Context, orgID, userID int64, role string) error {
	if role != "owner" && role != "member" {
		return fmt.Errorf("role must be owner or member")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO org_members(org_id,user_id,role) VALUES (?,?,?)
		ON CONFLICT(org_id,user_id) DO UPDATE SET role=excluded.role`, orgID, userID, role)
	return err
}

func (s *Store) RemoveOrgMember(ctx context.Context, orgID, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM org_members WHERE org_id=? AND user_id=?`, orgID, userID)
	return err
}

func (s *Store) OrgRole(ctx context.Context, orgID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT role FROM org_members WHERE org_id=? AND user_id=?`, orgID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// OrgMembership is an organization and a user's role in it.
type OrgMembership struct {
	Org  *User
	Role string // owner | member
}

// UserOrgs lists the organizations userID belongs to.
func (s *Store) UserOrgs(ctx context.Context, userID int64) ([]OrgMembership, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, u.full_name, u.email, u.is_admin, u.is_org, u.created_at, m.role
		FROM org_members m JOIN users u ON u.id=m.org_id WHERE m.user_id=? ORDER BY u.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMembership
	for rows.Next() {
		var u User
		var ts int64
		var m OrgMembership
		if err := rows.Scan(&u.ID, &u.Name, &u.FullName, &u.Email, &u.IsAdmin, &u.IsOrg, &ts, &m.Role); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(ts, 0)
		m.Org = &u
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) OrgMembers(ctx context.Context, orgID int64) ([]OrgMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, u.full_name, u.email, u.is_admin, u.is_org, u.created_at, m.role
		FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=? ORDER BY u.name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrgMember
	for rows.Next() {
		var u User
		var ts int64
		var m OrgMember
		if err := rows.Scan(&u.ID, &u.Name, &u.FullName, &u.Email, &u.IsAdmin, &u.IsOrg, &ts, &m.Role); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(ts, 0)
		m.User = &u
		out = append(out, m)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ keys

type SSHKey struct {
	ID          int64
	UserID      int64
	Title       string
	Fingerprint string
	PublicKey   string
	CreatedAt   time.Time
}

func (s *Store) AddSSHKey(ctx context.Context, userID int64, title, fingerprint, pub string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ssh_keys(user_id,title,fingerprint,public_key,created_at) VALUES (?,?,?,?,?)`,
		userID, title, fingerprint, pub, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("key is already registered")
	}
	return err
}

func (s *Store) DeleteSSHKey(ctx context.Context, userID, keyID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id=? AND user_id=?`, keyID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,user_id,title,fingerprint,public_key,created_at FROM ssh_keys WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var k SSHKey
		var ts int64
		if err := rows.Scan(&k.ID, &k.UserID, &k.Title, &k.Fingerprint, &k.PublicKey, &ts); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(ts, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) UserByFingerprint(ctx context.Context, fp string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT u.id, u.name, u.full_name, u.email, u.is_admin, u.is_org, u.created_at
		FROM users u JOIN ssh_keys k ON k.user_id=u.id WHERE k.fingerprint=?`, fp))
}

// Compact rewrites the database file and truncates the WAL so that deleted or
// overwritten content (e.g. plaintext secrets after encryption) no longer
// lingers in free pages.
func (s *Store) Compact(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

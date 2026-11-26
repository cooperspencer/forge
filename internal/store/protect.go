package store

import (
	"context"
	"strings"

	"github.com/cooperspencer/forge/internal/protect"
)

// Protections returns a repository's branch protection rules.
func (s *Store) Protections(ctx context.Context, repoID int64) ([]protect.Rule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pattern, push, allow_force, allow_delete, approvals, checks, block_changes, admin_bypass
		FROM branch_protections WHERE repo_id=? ORDER BY pattern`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protect.Rule
	for rows.Next() {
		var r protect.Rule
		var checks string
		if err := rows.Scan(&r.Pattern, &r.Push, &r.AllowForce, &r.AllowDelete, &r.Approvals, &checks, &r.BlockOnChanges, &r.AdminBypass); err != nil {
			return nil, err
		}
		if checks != "" {
			r.Checks = strings.Split(checks, ",")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveProtection creates or replaces the rule for r.Pattern.
func (s *Store) SaveProtection(ctx context.Context, repoID int64, r protect.Rule) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO branch_protections(repo_id,pattern,push,allow_force,allow_delete,approvals,checks,block_changes,admin_bypass,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(repo_id,pattern) DO UPDATE SET push=excluded.push, allow_force=excluded.allow_force, allow_delete=excluded.allow_delete,
			approvals=excluded.approvals, checks=excluded.checks, block_changes=excluded.block_changes, admin_bypass=excluded.admin_bypass`,
		repoID, r.Pattern, r.Push, b2i(r.AllowForce), b2i(r.AllowDelete), r.Approvals, strings.Join(r.Checks, ","), b2i(r.BlockOnChanges), b2i(r.AdminBypass), now())
	return err
}

func (s *Store) DeleteProtection(ctx context.Context, repoID int64, pattern string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM branch_protections WHERE repo_id=? AND pattern=?`, repoID, pattern)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// RoleInfo is one role (a column of the access matrix).
type RoleInfo struct {
	ID   int
	Name string
}

// PermissionInfo is one feature (a row of the access matrix).
type PermissionInfo struct {
	Key         string
	Description string
}

// Matrix is the role matrix: role name -> feature key -> allowed.
type Matrix map[string]map[string]bool

// LoadPageFlags returns the authentication switch of every page: page key -> login required.
func (s *Store) LoadPageFlags(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT page_key, requires_login FROM page_access`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		var v bool
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SavePageFlags stores the authentication switches (only for the page keys
// given) and writes the change to the audit log, in one transaction.
func (s *Store) SavePageFlags(ctx context.Context, flags map[string]bool, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := pageFlagsJSON(ctx, tx)
		if err != nil {
			return err
		}
		for key, v := range flags {
			if _, err := tx.Exec(ctx, `
				INSERT INTO page_access (page_key, requires_login, updated_at, updated_by)
				VALUES ($1, $2, now(), $3)
				ON CONFLICT (page_key) DO UPDATE
				SET requires_login = EXCLUDED.requires_login, updated_at = now(), updated_by = EXCLUDED.updated_by`,
				key, v, actor.ID); err != nil {
				return fmt.Errorf("save page %s: %w", key, err)
			}
		}
		after, err := pageFlagsJSON(ctx, tx)
		if err != nil {
			return err
		}
		if string(before) == string(after) {
			return nil // nothing changed: nothing to audit
		}
		return insertAudit(ctx, tx, AuditEntry{
			Actor: actor, Action: "access.pages", TargetType: "access", TargetID: "page_access",
			Outcome: "success", Reason: "authentication switches changed", Before: before, After: after,
		})
	})
}

func pageFlagsJSON(ctx context.Context, tx pgx.Tx) ([]byte, error) {
	rows, err := tx.Query(ctx, `SELECT page_key, requires_login FROM page_access ORDER BY page_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var k string
		var v bool
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func (s *Store) ListRoles(ctx context.Context) ([]RoleInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT role_id, role_name FROM roles ORDER BY role_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RoleInfo, error) {
		var x RoleInfo
		return x, r.Scan(&x.ID, &x.Name)
	})
}

func (s *Store) ListPermissions(ctx context.Context) ([]PermissionInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT permission_key, description FROM permissions ORDER BY permission_key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PermissionInfo, error) {
		var x PermissionInfo
		return x, r.Scan(&x.Key, &x.Description)
	})
}

// LoadMatrix returns which role may use which feature.
func (s *Store) LoadMatrix(ctx context.Context) (Matrix, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.role_name, rp.permission_key
		FROM role_permissions rp JOIN roles r ON r.role_id = rp.role_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := Matrix{}
	for rows.Next() {
		var role, perm string
		if err := rows.Scan(&role, &perm); err != nil {
			return nil, err
		}
		if m[role] == nil {
			m[role] = map[string]bool{}
		}
		m[role][perm] = true
	}
	return m, rows.Err()
}

// SaveMatrix replaces the whole role matrix with `m` (a role/feature pair
// that is not true in `m` is removed) and audits the before/after in one
// transaction. Unknown roles or features are ignored.
func (s *Store) SaveMatrix(ctx context.Context, m Matrix, actor Actor) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := matrixJSON(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM role_permissions`); err != nil {
			return err
		}
		for role, perms := range m {
			for perm, allowed := range perms {
				if !allowed {
					continue
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO role_permissions (role_id, permission_key)
					SELECT r.role_id, p.permission_key
					FROM roles r, permissions p
					WHERE r.role_name = $1 AND p.permission_key = $2
					ON CONFLICT DO NOTHING`, role, perm); err != nil {
					return fmt.Errorf("allow %s for %s: %w", perm, role, err)
				}
			}
		}
		after, err := matrixJSON(ctx, tx)
		if err != nil {
			return err
		}
		if string(before) == string(after) {
			return nil
		}
		return insertAudit(ctx, tx, AuditEntry{
			Actor: actor, Action: "access.roles", TargetType: "access", TargetID: "role_permissions",
			Outcome: "success", Reason: "role permissions changed", Before: before, After: after,
		})
	})
}

func matrixJSON(ctx context.Context, tx pgx.Tx) ([]byte, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.role_name, rp.permission_key
		FROM role_permissions rp JOIN roles r ON r.role_id = rp.role_id
		ORDER BY r.role_name, rp.permission_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string][]string{}
	for rows.Next() {
		var role, perm string
		if err := rows.Scan(&role, &perm); err != nil {
			return nil, err
		}
		m[role] = append(m[role], perm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, v := range m {
		sort.Strings(v)
	}
	return json.Marshal(m)
}

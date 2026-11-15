package authz

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// SyncCore upserts the core permission catalog, marks core permissions no
// longer in code as removed, seeds built-in roles (failing when a custom role
// holds a built-in key, builtinKeyConflict) and grants newly created
// core permissions and built-in roles (as assignable) to the built-in roles
// whose defaults include them. A built-in role whose defaults_rev is behind
// its rev (an upgrade from a core without it, or defaults that changed) is
// brought to its defaults once (advanceDefaults): reset to them when nobody
// holds it, otherwise given every default it lacks, nothing it holds taken
// away. Either way what an administrator changes afterwards stays as
// changed. Safe to run concurrently from several nodes; set-based to keep
// round trips low. The version is only bumped when something changed.
func (s *Service) SyncCore(ctx context.Context) error {
	defs := CorePermissions()
	var (
		keys, modules, labels, descs, scopeTypes, risks, requires store.Strings
		// int32 values, as the int columns; ::int[] narrows the array.
		sorts store.Int64s
	)
	for _, d := range defs {
		lb, err := json.Marshal(d.Label)
		if err != nil {
			return err
		}
		dj, err := json.Marshal(d.Description)
		if err != nil {
			return err
		}
		// A JSON array per row: unnest cannot take an array of arrays of
		// different lengths.
		rq, err := json.Marshal(append([]string{}, d.Requires...))
		if err != nil {
			return err
		}
		keys = append(keys, d.Key)
		modules = append(modules, d.Module)
		labels = append(labels, string(lb))
		descs = append(descs, string(dj))
		risks = append(risks, d.Risk)
		requires = append(requires, string(rq))
		sorts = append(sorts, int64(d.Sort))
		scopeTypes = append(scopeTypes, d.ScopeType)
	}
	var rKeys, rNames, rDescs store.Strings
	var rSuper, rCustom bools
	var rRevs store.Int64s // int32 values, as defaults_rev
	for _, r := range builtinRoles {
		n, _ := json.Marshal(r.name)
		d, _ := json.Marshal(r.description)
		rKeys = append(rKeys, r.key)
		rNames = append(rNames, string(n))
		rDescs = append(rDescs, string(d))
		rSuper = append(rSuper, r.superuser)
		rCustom = append(rCustom, r.assignCustom)
		rRevs = append(rRevs, int64(r.rev))
	}

	var changed bool
	var v int64
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		changed = false
		db := store.G(ctx, tx)
		if err := lockVersion(db).Error; err != nil {
			return err
		}
		if err := builtinKeyConflict(ctx, tx, rKeys); err != nil {
			return err
		}
		// Built-in roles: returns only rows created or fixed now.
		newRoles := map[string]bool{}
		err := scanKeyFlags(db.Raw(syncRolesSQL, rKeys, rNames, rDescs, rSuper, rCustom, rRevs), func(k string, inserted bool) {
			newRoles[k] = inserted
			changed = true
		})
		if err != nil {
			return err
		}
		// The roles getting every default they lack, with what the audit
		// says of why: created now, or behind their rev and held; and
		// those behind their rev that nobody holds, reset to them.
		fills := map[string]map[string]any{}
		for k, created := range newRoles {
			if created {
				fills[k] = map[string]any{"role_created": true}
			}
		}
		resets := map[string]bool{}
		if err := s.advanceDefaults(ctx, tx, rKeys, rRevs, rCustom, func(k string, rev int32, held bool) {
			if held {
				fills[k] = map[string]any{"defaults_rev": rev}
			} else {
				resets[k] = true
			}
			changed = true
		}); err != nil {
			return err
		}

		// Permissions: returns only inserted or modified rows.
		var inserted []string
		err = scanKeyFlags(db.Raw(syncPermissionsSQL, keys, modules, labels, descs, risks, requires, sorts, scopeTypes), func(k string, ins bool) {
			if ins {
				inserted = append(inserted, k)
			}
			changed = true
		})
		if err != nil {
			return err
		}

		// Built-in roles get their default permissions and assignable roles:
		// a reset writes them all (the permissions created above included),
		// the others get theirs through grantDefaults.
		for _, r := range builtinRoles {
			if resets[r.key] {
				if err := s.resetUnheld(ctx, tx, r); err != nil {
					return err
				}
				continue
			}
			fill := fills[r.key]
			// Scoped defaults are granted on a fill only: the actions exist
			// already, so none is new.
			if fill != nil && len(r.scoped) > 0 {
				if err := s.grantScopedDefaults(ctx, tx, r.key, r.scoped, fill); err != nil {
					return err
				}
			}
			if err := s.grantDefaults(ctx, tx, r.key, fill, r.unscopedDefaults(),
				func(k string) bool { return slices.Contains(inserted, k) }, "default", grantDefaultPermissionsSQL); err != nil {
				return err
			}
			if err := s.grantDefaults(ctx, tx, r.key, fill, r.assignable,
				func(k string) bool { return newRoles[k] }, "assignable", grantDefaultAssignableSQL); err != nil {
				return err
			}
		}

		removed := removeUndeclaredCore(db, keys)
		if removed.Error != nil {
			return removed.Error
		}
		changed = changed || removed.RowsAffected > 0
		if changed {
			if v, err = s.Bump(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		s.Committed(ctx, v)
	}
	return nil
}

// lockVersion locks the authz_meta row without bumping it: SyncCore
// serializes with every authz write and bumps only when it changed
// something.
func lockVersion(db *gorm.DB) *gorm.DB {
	return db.Exec(`SELECT version FROM authz_meta WHERE id = 1 FOR UPDATE`)
}

// syncRolesSQL creates the built-in roles of its arrays (key, name and
// description JSON, superuser, assign_custom, defaults_rev) and fixes the
// superuser flag of existing ones, returning the key of each row created
// or fixed and whether it was created.
const syncRolesSQL = `
INSERT INTO roles (key, name, description, builtin, superuser, assign_custom, defaults_rev)
SELECT k, n::jsonb, d::jsonb, true, su, ac, rev FROM unnest(?::text[], ?::text[], ?::text[], ?::bool[], ?::bool[], ?::int[]) AS t(k, n, d, su, ac, rev)
ON CONFLICT (key) DO UPDATE SET superuser = EXCLUDED.superuser
    WHERE roles.superuser IS DISTINCT FROM EXCLUDED.superuser
RETURNING key, (xmax = 0)`

// syncPermissionsSQL upserts the core permissions of its arrays (key,
// module, label and description JSON, risk, requires as a JSON array, sort,
// scope type) as active, returning the key of each row inserted or changed
// and whether it was inserted.
const syncPermissionsSQL = `
INSERT INTO permissions (key, module, label, description, source, risk, sensitive, requires, status, sort, scope_type)
SELECT k, m, l::jsonb, d::jsonb, 'core', ri, ri <> 'low', ARRAY(SELECT jsonb_array_elements_text(rq::jsonb)), 'active', so, st
FROM unnest(?::text[], ?::text[], ?::text[], ?::text[], ?::text[], ?::text[], ?::int[], ?::text[]) AS t(k, m, l, d, ri, rq, so, st)
ON CONFLICT (key) DO UPDATE SET module = EXCLUDED.module, label = EXCLUDED.label, description = EXCLUDED.description,
    risk = EXCLUDED.risk, sensitive = EXCLUDED.sensitive, requires = EXCLUDED.requires, status = 'active', sort = EXCLUDED.sort,
    scope_type = EXCLUDED.scope_type
    WHERE (permissions.module, permissions.label, permissions.description, permissions.risk, permissions.requires, permissions.status, permissions.sort, permissions.scope_type)
        IS DISTINCT FROM (EXCLUDED.module, EXCLUDED.label, EXCLUDED.description, EXCLUDED.risk, EXCLUDED.requires, 'active'::varchar, EXCLUDED.sort, EXCLUDED.scope_type)
RETURNING key, (xmax = 0)`

// grantDefaultPermissionsSQL grants a role (its key, then the permission
// keys) the permissions it lacks, returning what syncGrant reads.
const grantDefaultPermissionsSQL = `
WITH ins AS (
    INSERT INTO role_permissions (role_id, permission_id)
    SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = ? AND p.key = ANY(?)
    ON CONFLICT DO NOTHING RETURNING role_id, permission_id)
SELECT ins.role_id, p.key FROM ins JOIN permissions p ON p.id = ins.permission_id`

// grantDefaultAssignableSQL adds to the assignable list of a role (its key,
// then the role keys) the built-in, non-superuser roles it lacks, returning
// what syncGrant reads.
const grantDefaultAssignableSQL = `
WITH ins AS (
    INSERT INTO role_assignable_roles (role_id, assignable_role_id)
    SELECT r.id, a.id FROM roles r, roles a WHERE r.key = ? AND a.key = ANY(?) AND a.builtin AND NOT a.superuser
    ON CONFLICT DO NOTHING RETURNING role_id, assignable_role_id)
SELECT ins.role_id, a.key FROM ins JOIN roles a ON a.id = ins.assignable_role_id`

// removeUndeclaredCore marks removed the core permissions but keys.
func removeUndeclaredCore(db *gorm.DB, keys store.Strings) *gorm.DB {
	return db.Model(&model.Permission{}).Where("source = 'core' AND status <> 'removed' AND key <> ALL(?)", keys).Update("status", "removed")
}

// bools is a bool[] parameter: GORM expands a plain []bool into a list
// (see store.Int64s); nil is the empty array.
type bools []bool

// Value implements driver.Valuer.
func (a bools) Value() (driver.Value, error) {
	if a == nil {
		return []bool{}, nil
	}
	return []bool(a), nil
}

// builtinKeyConflict fails when a custom role holds one of the built-in role
// keys: the role is never turned into the built-in one, which would hand its
// holders that role's defaults, and never skipped, which would leave the
// built-in role missing. The error stops startup and says how to rename it.
func builtinKeyConflict(ctx context.Context, q store.Querier, builtinKeys []string) error {
	roles, err := queryRoles(ctx, q, `SELECT id, key, superuser FROM roles WHERE key = ANY(?) AND NOT builtin ORDER BY id`, store.Strings(builtinKeys))
	if err != nil || len(roles) == 0 {
		return err
	}
	msg := "authz: custom roles hold keys reserved for built-in roles; rename each one (any free key will do) and start again:"
	for _, r := range roles {
		msg += fmt.Sprintf("\n  role id %d key %q: UPDATE roles SET key = '%s_custom' WHERE id = %d;", r.id, r.key, r.key, r.id)
	}
	return errors.New(msg)
}

// advanceDefaults moves each built-in role of keys whose defaults_rev is
// behind its rev (revs) up to it and calls behind with the role, the rev
// reached and whether it is held (roleHolders; the super admin role, never
// reset, counts as held): SyncCore then grants a held role the defaults it
// lacks, turned assign_custom on here where its defaults have it (custom;
// audited), and resets one nobody holds (resetUnheld).
func (s *Service) advanceDefaults(ctx context.Context, tx pgx.Tx, keys store.Strings, revs store.Int64s, custom bools, behind func(key string, rev int32, held bool)) error {
	type moved struct {
		id     int64
		key    string
		rev    int32
		held   bool
		custom bool
	}
	var roles []moved
	if err := eachRow(advanceRoles(store.G(ctx, tx), keys, revs, custom), func(rows *sql.Rows) error {
		var m moved
		if err := rows.Scan(&m.id, &m.key, &m.rev, &m.held, &m.custom); err != nil {
			return err
		}
		roles = append(roles, m)
		return nil
	}); err != nil {
		return err
	}
	for _, m := range roles {
		behind(m.key, m.rev, m.held)
		if !m.custom {
			continue
		}
		if err := s.audit(ctx, tx, 0, "authz.sync.grant", "role", m.id, map[string]any{"via": "assignable", "assign_custom": true, "defaults_rev": m.rev}); err != nil {
			return err
		}
	}
	return nil
}

// advanceRoles moves the built-in roles of keys behind their rev (revs) to
// it, turning assign_custom on (custom) for the held ones, and returns the
// id, key, rev, whether held and whether assign_custom was turned on of
// each.
func advanceRoles(db *gorm.DB, keys store.Strings, revs store.Int64s, custom bools) *gorm.DB {
	return db.Raw(`
WITH behind AS (
    SELECT r.id, t.rev, r.superuser OR `+roleHolders+` > 0 AS held, t.ac AND NOT r.assign_custom AS custom
    FROM roles r JOIN unnest(?::text[], ?::int[], ?::bool[]) AS t(k, rev, ac) ON t.k = r.key
    WHERE r.builtin AND r.defaults_rev < t.rev)
UPDATE roles r SET defaults_rev = b.rev, assign_custom = r.assign_custom OR (b.held AND b.custom)
FROM behind b WHERE r.id = b.id
RETURNING r.id, r.key, b.rev, b.held, b.held AND b.custom`, keys, revs, custom)
}

// resetUnheld resets the built-in role def, behind its rev with nobody
// holding it, to its defaults as ResetRole does (resetDefaults, by the
// system, audited with via sync and the rev): no user's rights change, and
// whoever is given the role next gets what its defaults say.
func (s *Service) resetUnheld(ctx context.Context, tx pgx.Tx, def builtinRole) error {
	r, err := readRole(selectRoles(store.G(ctx, tx), ` WHERE r.key = ?`, def.key))
	if err != nil {
		return err
	}
	return s.resetDefaults(ctx, tx, 0, r, def, map[string]any{"via": "sync", "defaults_rev": def.rev})
}

// grantDefaults grants the built-in role roleKey its defaults through query
// (its placeholders the role key, then the defaults; returning what
// syncGrant reads). On a fill (a role created now, or behind its rev) it
// gets every one it lacks, otherwise only the defaults created now; every
// grant is audited with via and the fill's detail (role_created or
// defaults_rev), so a role the sync creates or fills in on an upgrade shows
// what it was given.
func (s *Service) grantDefaults(ctx context.Context, tx pgx.Tx, roleKey string, fill map[string]any, defaults []string, created func(string) bool, via, query string) error {
	if fill == nil {
		defaults = slices.DeleteFunc(slices.Clone(defaults), func(k string) bool { return !created(k) })
	}
	if len(defaults) == 0 {
		return nil
	}
	return s.syncGrant(ctx, tx, withVia(fill, via), query, roleKey, store.Strings(defaults))
}

// withVia is the audit detail of a sync grant: via plus the fill's detail.
func withVia(fill map[string]any, via string) map[string]any {
	detail := maps.Clone(fill)
	if detail == nil {
		detail = map[string]any{}
	}
	detail["via"] = via
	return detail
}

// grantScopedDefaults grants the built-in role roleKey the scoped defaults
// (canonical keys) it lacks, each audited as authz.sync.grant (via default
// and the fill's detail). One the role's unscoped grant of the same action
// implies is left out, as migration 0097 leaves out a redundant own scope.
func (s *Service) grantScopedDefaults(ctx context.Context, tx pgx.Tx, roleKey string, keys []string, fill map[string]any) error {
	// The keys come first in the text, the role key second.
	return s.syncGrant(ctx, tx, withVia(fill, "default"), grantScopedDefaultsSQL, store.Strings(keys), roleKey)
}

// grantScopedDefaultsSQL grants a role the scoped defaults (canonical keys,
// the first parameter) it lacks, those its unscoped grant of the action
// implies aside; the role key is the second parameter. It returns what
// syncGrant reads.
const grantScopedDefaultsSQL = `
WITH ins AS (
    INSERT INTO role_permission_scopes (role_id, permission_id, scope)
    SELECT r.id, p.id, d.scope FROM roles r,
        (SELECT substring(k FROM '^(.*):[^:]*$') AS action, substring(k FROM '[^:]*$') AS scope FROM unnest(?::text[]) k) d
    JOIN permissions p ON p.key = d.action AND p.source = 'core'
    WHERE r.key = ?
      AND NOT EXISTS (SELECT 1 FROM role_permissions f WHERE f.role_id = r.id AND f.permission_id = p.id)
    ON CONFLICT DO NOTHING RETURNING role_id, permission_id, scope)
SELECT ins.role_id, p.key || ':' || ins.scope FROM ins JOIN permissions p ON p.id = ins.permission_id`

// syncGrant runs query, an automatic grant returning the role id and the
// granted key (a permission, or an assignable role) of each row it adds,
// and audits each row as authz.sync.grant by the system with via plus the
// key as detail.
func (s *Service) syncGrant(ctx context.Context, tx pgx.Tx, via map[string]any, query string, args ...any) error {
	type grant struct {
		role int64
		key  string
	}
	var grants []grant
	if err := eachRow(store.G(ctx, tx).Raw(query, args...), func(rows *sql.Rows) error {
		var g grant
		if err := rows.Scan(&g.role, &g.key); err != nil {
			return err
		}
		grants = append(grants, g)
		return nil
	}); err != nil {
		return err
	}
	for _, g := range grants {
		detail := maps.Clone(via)
		detail["key"] = g.key
		if err := s.audit(ctx, tx, 0, "authz.sync.grant", "role", g.role, detail); err != nil {
			return err
		}
	}
	return nil
}

// scanKeyFlags calls fn with each (key, flag) row of q.
func scanKeyFlags(q *gorm.DB, fn func(key string, flag bool)) error {
	return eachRow(q, func(rows *sql.Rows) error {
		var k string
		var f bool
		if err := rows.Scan(&k, &f); err != nil {
			return err
		}
		fn(k, f)
		return nil
	})
}

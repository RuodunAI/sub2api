package migrations_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

const casbinRule = "0100_casbin_rule.sql"

// policy lists casbin_rule as "<ptype> <v0> <v1> <v2>", sorted bytewise;
// with role keys, the rows of those roles only (p rows by subject, g rows
// by role).
func (h *hardeningDB) policy(roles ...string) string {
	h.t.Helper()
	return h.text(`SELECT COALESCE(string_agg(concat_ws(' ', ptype, v0, v1, NULLIF(v2, '')), ', ' ORDER BY ptype COLLATE "C", v0 COLLATE "C", v1 COLLATE "C", v2 COLLATE "C"), '')
        FROM casbin_rule WHERE cardinality($1::text[]) = 0 OR CASE ptype WHEN 'p' THEN v0 ELSE v1 END = ANY($1::text[])`,
		store.Strings(roleSubs(roles)))
}

func roleSubs(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = "r:" + k
	}
	return out
}

// noDrift fails unless casbin_rule is exactly the projection of the role
// tables after step.
func (h *hardeningDB) noDrift(step string) {
	h.t.Helper()
	require.Equal(h.t, "", h.drift(), "drift after %s", step)
}

// drift lists authz_casbin_drift as "<kind> <ptype> <v0> <v1> <v2>".
func (h *hardeningDB) drift() string {
	h.t.Helper()
	return h.text(`SELECT COALESCE(string_agg(concat_ws(' ', kind, ptype, v0, v1, NULLIF(v2, '')), ', ' ORDER BY kind, ptype, v0, v1, v2 COLLATE "C"), '') FROM authz_casbin_drift`)
}

func (h *hardeningDB) id(sql string, args ...any) (id int64) {
	h.t.Helper()
	require.NoError(h.t, h.db.SQL.QueryRowContext(h.ctx, sql, args...).Scan(&id))
	return id
}

// tx runs fn in a transaction, as the callers of the tx-taking authz
// methods do.
func (h *hardeningDB) tx(fn func(tx *gorm.DB) error) {
	h.t.Helper()
	require.NoError(h.t, h.db.Tx(h.ctx, fn))
}

// casbinService is a started authz service on a fresh database (the core
// sync wrote the catalog and the built-in roles) whose super admin is root.
func casbinService(t *testing.T) (h *hardeningDB, s *authz.Service, root int64) {
	h = &hardeningDB{t: t, db: testutil.DB(t), ctx: context.Background()}
	s = authz.New(authz.Deps{DB: h.db})
	require.NoError(t, s.Start(h.ctx))
	h.noDrift("core sync")
	root = h.id(`INSERT INTO users (email, password_hash) VALUES ('root@x', '') RETURNING id`)
	h.tx(func(tx *gorm.DB) error { _, _, err := s.TransferSuperAdmin(h.ctx, tx, 0, root); return err })
	return h, s, root
}

// Every kind of authz write, through the Go write path where there is one,
// leaves casbin_rule exactly the projection of the role tables: what each
// step writes is there, what it removes is gone, and the drift view is
// empty after each.
func TestCasbinRuleFollowsWriters(t *testing.T) {
	h, s, root := casbinService(t)
	ctx := h.ctx
	sub := func(uid int64) string { return "u:" + strconv.FormatInt(uid, 10) }
	require.Equal(t, "g "+sub(root)+" r:super_admin, p r:super_admin * *", h.policy(authz.RoleSuperAdmin))
	alice := h.id(`INSERT INTO users (email, password_hash) VALUES ('alice@x', '') RETURNING id`)
	bob := h.id(`INSERT INTO users (email, password_hash) VALUES ('bob@x', '') RETURNING id`)

	// A new role with unscoped and scoped grants.
	r, err := s.CreateRole(ctx, root, authz.CreateRoleInput{Key: "auditor", Name: core.LocalizedText{"en": "Auditor"},
		PermissionKeys: []string{"account:read", "account:test:group/12", "account:update:group/12/account/5", "proxy:read:own"}})
	require.NoError(t, err)
	require.Equal(t, "p r:auditor account:read *, p r:auditor account:test group/12, p r:auditor account:update group/12/account/5, p r:auditor proxy:read own",
		h.policy("auditor"))
	h.noDrift("create role")

	// Setting a user's roles, then assigning more and taking one away.
	setRoles := func(uid int64, keys ...string) {
		h.tx(func(tx *gorm.DB) error { _, err := s.SetUserRoles(ctx, tx, root, uid, keys); return err })
	}
	setRoles(alice, authz.RoleUser, "auditor")
	setRoles(bob, authz.RoleUser)
	h.noDrift("set user roles")
	setRoles(bob, authz.RoleUser, authz.RoleOperator, "auditor")
	setRoles(alice, "auditor")
	require.Equal(t, "g "+sub(alice)+" r:auditor, g "+sub(bob)+" r:auditor, p r:auditor account:read *, p r:auditor account:test group/12, "+
		"p r:auditor account:update group/12/account/5, p r:auditor proxy:read own", h.policy("auditor"))
	require.NotContains(t, h.policy(authz.RoleUser), sub(alice))
	h.noDrift("assign roles")

	// Clearing the grants naming a deleted group.
	h.tx(func(tx *gorm.DB) error {
		_, err := core.ClearScopeGrants(ctx, tx, root, core.ScopeRefGroup, []int64{12})
		return err
	})
	require.NotContains(t, h.policy("auditor"), "group/12")
	h.noDrift("clear scope grants")

	// Revoking a grant.
	_, err = s.SetRolePermissions(ctx, root, r.ID, []string{"account:read"}, nil)
	require.NoError(t, err)
	require.Equal(t, "p r:auditor account:read *", h.text(`SELECT string_agg(concat_ws(' ', ptype, v0, v1, v2), ', ') FROM casbin_rule WHERE ptype = 'p' AND v0 = 'r:auditor'`))
	h.noDrift("revoke grant")

	// Handing the super admin role over: root becomes an admin.
	h.tx(func(tx *gorm.DB) error { _, _, err := s.TransferSuperAdmin(ctx, tx, root, alice); return err })
	require.Equal(t, "g "+sub(alice)+" r:super_admin, p r:super_admin * *", h.policy(authz.RoleSuperAdmin))
	require.Contains(t, h.policy(authz.RoleAdmin), "g "+sub(root)+" r:admin")
	h.noDrift("transfer super admin")

	// Deleting a custom role: its grants and assignments cascade.
	require.NoError(t, s.DeleteRole(ctx, alice, r.ID))
	require.Equal(t, "", h.policy("auditor"))
	h.noDrift("delete role")

	// Deleting a user for good (nothing in Go does; the FK cascades).
	h.exec(`DELETE FROM users WHERE id = $1`, bob)
	require.Equal(t, "0", h.text(`SELECT count(*)::text FROM casbin_rule WHERE ptype = 'g' AND v0 = $1`, sub(bob)))
	h.noDrift("hard delete user")

	// Plugin permissions: created with grants by the sync, scoped too; one
	// plugin's are deleted by the catalog, the other's cascade from its row
	// (uninstall), with their grants.
	for _, key := range []string{"pa", "pb"} {
		h.exec(`INSERT INTO plugins (key, name, status) VALUES ($1, '{}', 'enabled')`, key)
		h.tx(func(tx *gorm.DB) error {
			return s.SyncPlugin(ctx, tx, key, []core.PermissionDef{{Key: "run", Module: "plugin." + key, Label: core.LocalizedText{"en": "Run"},
				GrantRoles: map[string][]string{authz.RoleOperator: {""}, authz.RoleUser: {""}}}})
		})
		h.exec(`INSERT INTO role_permission_scopes (role_id, permission_id, scope)
            SELECT r.id, p.id, 'flow/3' FROM roles r, permissions p WHERE r.key = 'admin' AND p.key = $1`, "plugin."+key+":run")
	}
	require.Contains(t, h.policy(authz.RoleAdmin), "p r:admin plugin.pb:run flow/3")
	require.Contains(t, h.policy(authz.RoleOperator), "p r:operator plugin.pa:run *")
	h.noDrift("plugin sync")
	h.tx(func(tx *gorm.DB) error { return s.DeletePlugin(ctx, tx, "pa") })
	h.tx(func(tx *gorm.DB) error {
		return store.G(ctx, tx).Where("key = ? AND NOT builtin", "pb").Delete(&model.Plugin{}).Error
	})
	require.Equal(t, "0", h.text(`SELECT count(*)::text FROM casbin_rule WHERE v1 LIKE 'plugin.%'`))
	h.noDrift("uninstall plugins")

	// The core sync again and a reset of a built-in role.
	require.NoError(t, s.SyncCore(ctx))
	_, err = s.ResetRole(ctx, alice, h.id(`SELECT id FROM roles WHERE key = 'operator'`))
	require.NoError(t, err)
	h.noDrift("core sync, reset role")
}

// Writes no Go path makes but SQL may: updates of grant and assignment rows,
// the superuser flag, a grant held both unscoped and as a '*' scope row, a
// role rename (SyncCore's start-up error asks for one), a same-key upsert of
// a permission; a permission rename is refused.
func TestCasbinRuleFollowsSQL(t *testing.T) {
	h, _, root := casbinService(t)
	h.exec(`INSERT INTO roles (key, name) VALUES ('ed', '{}'), ('vw', '{}');
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, 'own' FROM roles r, permissions p WHERE r.key = 'ed' AND p.key = 'account:read';
INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u, roles r WHERE u.email = 'root@x' AND r.key = 'ed'`)
	h.noDrift("insert")

	h.exec(`UPDATE role_permission_scopes SET scope = 'group/7' WHERE scope = 'own'`)
	h.exec(`UPDATE user_roles SET role_id = (SELECT id FROM roles WHERE key = 'vw') WHERE role_id = (SELECT id FROM roles WHERE key = 'ed')`)
	require.Equal(t, "p r:ed account:read group/7", h.policy("ed"))
	require.Equal(t, "g u:"+strconv.FormatInt(root, 10)+" r:vw", h.policy("vw"))
	h.noDrift("update rows")

	h.exec(`UPDATE roles SET superuser = false WHERE key = 'super_admin'`)
	require.Equal(t, "0", h.text(`SELECT count(*)::text FROM casbin_rule WHERE v1 = '*'`))
	h.exec(`UPDATE roles SET superuser = true WHERE key = 'super_admin'`)
	require.Equal(t, "1", h.text(`SELECT count(*)::text FROM casbin_rule WHERE v1 = '*'`))
	h.noDrift("superuser flag")

	// Both an unscoped grant and a '*' scope row project to one p row,
	// which stays until neither holds it, whatever the order.
	both := `INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = 'ed' AND p.key = 'account:test';
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, '*' FROM roles r, permissions p WHERE r.key = 'ed' AND p.key = 'account:test'`
	for _, order := range [][2]string{{"role_permissions", "role_permission_scopes"}, {"role_permission_scopes", "role_permissions"}} {
		h.exec(both)
		h.exec(`DELETE FROM ` + order[0] + ` WHERE role_id = (SELECT id FROM roles WHERE key = 'ed') AND permission_id = (SELECT id FROM permissions WHERE key = 'account:test')`)
		require.Contains(t, h.policy("ed"), "p r:ed account:test *", "after deleting from %s", order[0])
		h.noDrift("one of two")
		h.exec(`DELETE FROM ` + order[1] + ` WHERE role_id = (SELECT id FROM roles WHERE key = 'ed') AND permission_id = (SELECT id FROM permissions WHERE key = 'account:test')`)
		require.NotContains(t, h.policy("ed"), "account:test")
		h.noDrift("both")
	}

	// A statement deleting a grant and adding it back keeps it.
	h.exec(`WITH d AS (DELETE FROM role_permission_scopes WHERE scope = 'group/7' RETURNING role_id, permission_id, scope)
        INSERT INTO role_permission_scopes SELECT * FROM d`)
	require.Equal(t, "p r:ed account:read group/7", h.policy("ed"))
	h.noDrift("delete and insert in one statement")

	h.exec(`UPDATE roles SET key = 'vw_custom' WHERE key = 'vw'`)
	require.Equal(t, "", h.policy("vw"))
	require.Equal(t, "g u:"+strconv.FormatInt(root, 10)+" r:vw_custom", h.policy("vw_custom"))
	h.noDrift("role rename")

	h.exec(`INSERT INTO permissions (key, module, label, source) VALUES ('account:read', 'account', '{}', 'core')
        ON CONFLICT (key) DO UPDATE SET key = EXCLUDED.key`)
	_, err := h.db.SQL.ExecContext(h.ctx, `UPDATE permissions SET key = 'account:peek' WHERE key = 'account:read'`)
	require.ErrorContains(t, err, "permissions.key is immutable")
	h.noDrift("permission rename refused")
}

// A rolled back write leaves no policy row behind, nor takes one away.
func TestCasbinRuleRollback(t *testing.T) {
	h, s, root := casbinService(t)
	before := h.policy()
	errRollback := errors.New("rollback")
	err := h.db.Tx(h.ctx, func(tx *gorm.DB) error {
		if _, err := s.SetUserRoles(h.ctx, tx, root, root, []string{authz.RoleSuperAdmin, authz.RoleUser, authz.RoleOperator}); err != nil {
			return err
		}
		if err := store.G(h.ctx, tx).Exec(`DELETE FROM role_permissions WHERE role_id = (SELECT id FROM roles WHERE key = 'user')`).Error; err != nil {
			return err
		}
		if err := store.G(h.ctx, tx).Exec(`INSERT INTO roles (key, name, superuser) VALUES ('ghost', '{}', false)`).Error; err != nil {
			return err
		}
		var n int64
		if err := store.G(h.ctx, tx).Raw(`SELECT count(*) FROM casbin_rule WHERE v1 = 'r:operator' OR v0 = 'r:user'`).Scan(&n).Error; err != nil {
			return err
		}
		require.EqualValues(t, 1, n, "inside the transaction: the new assignment, no user grants")
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	require.Equal(t, before, h.policy())
	h.noDrift("rollback")
}

// TRUNCATE fires no row trigger: the statement trigger rebuilds the
// projection of whatever was truncated, casbin_rule itself included, and
// a parent's TRUNCATE ... CASCADE works.
func TestCasbinRuleTruncate(t *testing.T) {
	h, _, _ := casbinService(t)
	all := h.policy()
	require.NotEqual(t, "", all)

	h.exec(`TRUNCATE casbin_rule`)
	require.Equal(t, all, h.policy(), "casbin_rule is rebuilt")
	h.exec(`TRUNCATE role_permission_scopes`)
	require.Equal(t, "0", h.text(`SELECT count(*)::text FROM casbin_rule WHERE ptype = 'p' AND v2 <> '*'`))
	h.noDrift("truncate role_permission_scopes")
	h.exec(`TRUNCATE user_roles`)
	require.Equal(t, "0", h.text(`SELECT count(*)::text FROM casbin_rule WHERE ptype = 'g'`))
	h.noDrift("truncate user_roles")
	h.exec(`TRUNCATE plugins CASCADE`) // permissions, so every grant, with it
	require.Equal(t, "p r:super_admin * *", h.policy())
	h.exec(`TRUNCATE roles CASCADE`)
	require.Equal(t, "", h.policy())
	h.noDrift("truncate roles")
}

// The drift view tells rows missing from casbin_rule from extra ones, and
// authz_casbin_repair fixes both, returning how many rows it changed.
func TestCasbinRuleRepair(t *testing.T) {
	h, _, _ := casbinService(t)
	all := h.policy()
	// casbin_rule has no row triggers: these bypass the projection, as
	// would a write with session_replication_role = replica.
	h.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'r:user' AND v1 = 'gateway:use'`)
	h.exec(`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('p', 'r:user', 'account:delete', '*'), ('g', 'u:999', 'r:admin', '')`)
	h.tx(func(tx *gorm.DB) error {
		return store.G(h.ctx, tx).Exec(`SET LOCAL session_replication_role = replica;
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, 'own' FROM roles r, permissions p WHERE r.key = 'user' AND p.key = 'proxy:read'`).Error
	})
	require.Equal(t, "extra g u:999 r:admin, extra p r:user account:delete *, missing p r:user gateway:use *, missing p r:user proxy:read own", h.drift())

	require.Equal(t, "4", h.text(`SELECT authz_casbin_repair()::text`))
	h.noDrift("repair")
	require.Equal(t, strings.Join(sorted(append(strings.Split(all, ", "), "p r:user proxy:read own")...), ", "), h.policy())
	require.Equal(t, "0", h.text(`SELECT authz_casbin_repair()::text`))
}

// casbin_rule's columns hold the longest key, role subject and scope the
// role tables allow (a widened source column must widen them too).
func TestCasbinRuleColumnWidths(t *testing.T) {
	h, _, _ := casbinService(t)
	width := func(table, column string) int64 {
		return h.id(`SELECT character_maximum_length FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
	}
	role, key, scope := width("roles", "key"), width("permissions", "key"), width("role_permission_scopes", "scope")
	require.GreaterOrEqual(t, width("casbin_rule", "v0"), 2+role)
	require.GreaterOrEqual(t, width("casbin_rule", "v1"), max(key, 2+role))
	require.GreaterOrEqual(t, width("casbin_rule", "v2"), scope)

	rk, pk, sc := strings.Repeat("r", int(role)), strings.Repeat("k", int(key)), strings.Repeat("s", int(scope))
	h.exec(`INSERT INTO roles (key, name, superuser) VALUES ($1, '{}', false)`, rk)
	h.exec(`INSERT INTO permissions (key, module, label, source) VALUES ($1, 'k', '{}', 'core')`, pk)
	h.exec(`INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, $3 FROM roles r, permissions p WHERE r.key = $1 AND p.key = $2`, rk, pk, sc)
	h.exec(`INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u, roles r WHERE u.email = 'root@x' AND r.key = $1`, rk)
	require.Equal(t, "g u:"+h.text(`SELECT id::text FROM users WHERE email = 'root@x'`)+" r:"+rk+", p r:"+rk+" "+pk+" "+sc, h.policy(rk))
	h.noDrift("longest values")
}

// An upgrade: a database as 0.1.289 leaves it (through 0099) gets the
// projection of what it holds, grants of removed, disabled and staged
// permissions and soft-deleted users included; the version moves once; a
// second run changes nothing.
func TestCasbinRuleMigration(t *testing.T) {
	h := &hardeningDB{t: t, db: testutil.DBThrough(t, "0099_plugin_permission_meta.sql"), ctx: context.Background()}
	h.exec(`
INSERT INTO roles (key, name, builtin, superuser) VALUES ('super_admin', '{}', true, true), ('user', '{}', true, false), ('editor', '{}', false, false);
INSERT INTO plugins (key, name, status) VALUES ('pm', '{}', 'disabled');
INSERT INTO permissions (key, module, label, source, plugin_key, status) VALUES
    ('gateway:use', 'gateway', '{}', 'core', NULL, 'active'), ('account:read', 'account', '{}', 'core', NULL, 'active'),
    ('core:gone', 'core', '{}', 'core', NULL, 'removed'), ('plugin.pm:run', 'plugin.pm', '{}', 'plugin', 'pm', 'disabled'),
    ('plugin.pm:next', 'plugin.pm', '{}', 'plugin', 'pm', 'staged');
INSERT INTO users (email, password_hash, status, deleted_at) VALUES ('root@x', '', 'active', NULL), ('off@x', '', 'disabled', NULL), ('gone@x', '', 'active', now());
INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r JOIN permissions p ON (r.key, p.key) IN (
    ('user', 'gateway:use'), ('editor', 'core:gone'), ('editor', 'plugin.pm:run'), ('editor', 'plugin.pm:next'));
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, s FROM roles r JOIN permissions p ON p.key = 'account:read'
    CROSS JOIN (VALUES ('own'), ('group/12/account/5')) v(s) WHERE r.key = 'editor';
INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u JOIN roles r ON (u.email, r.key) IN (
    ('root@x', 'super_admin'), ('off@x', 'user'), ('off@x', 'editor'), ('gone@x', 'user'))`)
	uid := func(email string) string { return h.text(`SELECT id::text FROM users WHERE email = $1`, email) }
	version := h.text(`SELECT version::text FROM authz_meta`)

	require.NoError(t, h.migrate())
	want := strings.Join(sorted(
		"g u:"+uid("gone@x")+" r:user", "g u:"+uid("off@x")+" r:editor", "g u:"+uid("off@x")+" r:user", "g u:"+uid("root@x")+" r:super_admin",
		"p r:editor account:read group/12/account/5", "p r:editor account:read own", "p r:editor core:gone *",
		"p r:editor plugin.pm:next *", "p r:editor plugin.pm:run *", "p r:super_admin * *", "p r:user gateway:use *"), ", ")
	require.Equal(t, want, h.policy())
	h.noDrift("migration")
	require.Equal(t, version, h.text(`SELECT (version - 1)::text FROM authz_meta`), "the version moves once")

	migrated := h.text(`SELECT version::text FROM authz_meta`)
	h.exec(`DELETE FROM schema_migrations WHERE id = $1`, casbinRule)
	require.NoError(t, h.migrate())
	require.Equal(t, want, h.policy())
	require.Equal(t, migrated, h.text(`SELECT version::text FROM authz_meta`), "a second run moved the version")
	h.noDrift("second run")
}

// sorted is the strings ss, sorted (as policy lists them).
func sorted(ss ...string) []string {
	slices.Sort(ss)
	return ss
}

// The rollback script puts the database back in the shape 0.1.289 runs on
// (the schema of a database migrated through 0099), the role tables as
// they were; once only, and upgrading again projects them anew.
func TestCasbinRuleDown(t *testing.T) {
	h := &hardeningDB{t: t, db: testutil.DBThrough(t, "0099_plugin_permission_meta.sql"), ctx: context.Background()}
	h.exec(`INSERT INTO roles (key, name, superuser) VALUES ('super_admin', '{}', true), ('editor', '{}', false);
INSERT INTO permissions (key, module, label, source) VALUES ('account:read', 'account', '{}', 'core');
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, 'own' FROM roles r, permissions p WHERE r.key = 'editor'`)
	schema := h.schema()
	require.NoError(t, h.migrate())
	h.exec(`INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r, permissions p WHERE r.key = 'editor'`)
	grants, policy := h.grants(), h.policy()
	version := h.text(`SELECT version::text FROM authz_meta`)

	require.NoError(t, h.down("0.1.290-casbin-down.sql"))
	require.Equal(t, schema, h.schema())
	require.Equal(t, grants, h.grants())
	require.Equal(t, version, h.text(`SELECT (version - 1)::text FROM authz_meta`), "the version moves once")
	require.ErrorContains(t, h.down("0.1.290-casbin-down.sql"), "nothing to roll back")

	require.NoError(t, h.migrate())
	require.Equal(t, policy, h.policy())
}

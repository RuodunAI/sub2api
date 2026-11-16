package migrations_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

const (
	// beforeCasbin is the last core migration before 0100, which turns its
	// role tables into casbin_rule (0.1.290 runs 0099-0101 in one upgrade).
	beforeCasbin = "0099_plugin_permission_meta.sql"
	casbinRule   = "0100_casbin_rule.sql"
)

// policy lists casbin_rule as "<ptype> <v0> <v1> <v2>", sorted bytewise;
// with role keys, the rows of those roles only (p rows by subject, g rows
// by role).
func (h *hardeningDB) policy(roles ...string) string {
	h.t.Helper()
	subs := make([]string, len(roles))
	for i, k := range roles {
		subs[i] = casbinrule.RoleSub(k)
	}
	return h.text(`SELECT COALESCE(string_agg(concat_ws(' ', ptype, v0, v1, NULLIF(v2, '')), ', ' ORDER BY ptype COLLATE "C", v0 COLLATE "C", v1 COLLATE "C", v2 COLLATE "C"), '')
        FROM casbin_rule WHERE cardinality($1::text[]) = 0 OR CASE ptype WHEN 'p' THEN v0 ELSE v1 END = ANY($1::text[])`,
		store.Strings(subs))
}

func (h *hardeningDB) id(sql string, args ...any) (id int64) {
	h.t.Helper()
	require.NoError(h.t, h.db.SQL.QueryRowContext(h.ctx, sql, args...).Scan(&id))
	return id
}

// casbinService is a started authz service on a fresh database (the core
// sync wrote the catalog and the built-in roles) whose super admin is root.
func casbinService(t *testing.T) (h *hardeningDB, s *authz.Service, root int64) {
	h = &hardeningDB{t: t, db: testutil.DB(t), ctx: context.Background()}
	s = authz.New(authz.Deps{DB: h.db})
	require.NoError(t, s.Start(h.ctx))
	root = h.id(`INSERT INTO users (email, password_hash) VALUES ('root@x', '') RETURNING id`)
	require.NoError(t, h.db.Tx(h.ctx, func(tx *gorm.DB) error { _, _, err := s.TransferSuperAdmin(h.ctx, tx, 0, root); return err }))
	return h, s, root
}

// casbin_rule's columns hold the longest role subject, permission key and
// scope (role_permission_scopes.scope was varchar(200)): a widened
// roles.key or permissions.key must widen them too.
func TestCasbinRuleColumnWidths(t *testing.T) {
	h, _, root := casbinService(t)
	width := func(table, column string) int64 {
		return h.id(`SELECT character_maximum_length FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
	}
	const scope = 200
	role, key := width("roles", "key"), width("permissions", "key")
	require.GreaterOrEqual(t, width("casbin_rule", "v0"), max(2+role, int64(len(casbinrule.UserSub(1<<62)))))
	require.GreaterOrEqual(t, width("casbin_rule", "v1"), max(key, 2+role))
	require.GreaterOrEqual(t, width("casbin_rule", "v2"), int64(scope))

	rk, pk, sc := strings.Repeat("r", int(role)), strings.Repeat("k", int(key)), strings.Repeat("s", scope)
	h.exec(`INSERT INTO roles (key, name, superuser) VALUES ($1, '{}', false)`, rk)
	h.exec(`INSERT INTO permissions (key, module, label, source) VALUES ($1, 'k', '{}', 'core')`, pk)
	testutil.Grant(t, h.db, rk, pk, sc)
	testutil.AssignRoles(t, h.db, root, rk)
	require.Equal(t, "g "+casbinrule.UserSub(root)+" r:"+rk+", p r:"+rk+" "+pk+" "+sc, h.policy(rk))
}

// An upgrade: a database through 0099 (as develop's 0.1.289 builds left
// it) gets the rows of what it holds, grants of removed, disabled and
// staged permissions and soft-deleted users included, a grant held both
// for every row and as a scope * row once; the role tables and 0097's
// one-holder trigger go; the version moves once; a second run changes
// nothing.
func TestCasbinRuleMigration(t *testing.T) {
	h := &hardeningDB{t: t, db: testutil.DBThrough(t, beforeCasbin), ctx: context.Background()}
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
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, s FROM roles r JOIN permissions p ON (r.key, p.key) IN (
    ('editor', 'account:read'), ('user', 'gateway:use'))
    CROSS JOIN (VALUES ('own'), ('group/12/account/5'), ('*')) v(s) WHERE r.key = 'editor' OR s = '*';
INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u JOIN roles r ON (u.email, r.key) IN (
    ('root@x', 'super_admin'), ('off@x', 'user'), ('off@x', 'editor'), ('gone@x', 'user'))`)
	uid := func(email string) string { return h.text(`SELECT id::text FROM users WHERE email = $1`, email) }
	version := h.text(`SELECT version::text FROM authz_meta`)

	require.NoError(t, h.migrate())
	want := strings.Join(sorted(
		"g u:"+uid("gone@x")+" r:user", "g u:"+uid("off@x")+" r:editor", "g u:"+uid("off@x")+" r:user", "g u:"+uid("root@x")+" r:super_admin",
		"p r:editor account:read *", "p r:editor account:read group/12/account/5", "p r:editor account:read own", "p r:editor core:gone *",
		"p r:editor plugin.pm:next *", "p r:editor plugin.pm:run *", "p r:super_admin * *", "p r:user gateway:use *"), ", ")
	require.Equal(t, want, h.policy())
	require.Equal(t, "", h.text(`SELECT concat_ws(' ', to_regclass('role_permissions'), to_regclass('role_permission_scopes'), to_regclass('user_roles'),
        to_regprocedure('user_roles_one_superuser()'))`), "the role tables and their trigger function are gone")
	require.Equal(t, "", h.text(`SELECT COALESCE(string_agg(proname, ', '), '') FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'authz_casbin%'`))
	require.Equal(t, version, h.text(`SELECT (version - 1)::text FROM authz_meta`), "the version moves once")

	migrated := h.text(`SELECT version::text FROM authz_meta`)
	h.exec(`DELETE FROM schema_migrations WHERE id = $1`, casbinRule)
	require.NoError(t, h.migrate())
	require.Equal(t, want, h.policy())
	require.Equal(t, migrated, h.text(`SELECT version::text FROM authz_meta`), "a second run moved the version")

	// One super admin, as user_roles_one_superuser kept it.
	err := casbinrule.AddMembers(h.db.Gorm, authz.RoleSuperAdmin, []int64{h.id(`SELECT id FROM users WHERE email = 'off@x'`)})
	require.True(t, store.IsUniqueViolation(err, "casbin_rule_one_superuser"), "a second super admin: %v", err)
}

// sorted is the strings ss, sorted (as policy lists them).
func sorted(ss ...string) []string {
	slices.Sort(ss)
	return ss
}

// The migration is one way: the old down script only says so and changes
// nothing (the way back is the backup taken before it).
func TestCasbinRuleOneWay(t *testing.T) {
	h, _, _ := casbinService(t)
	schema, policy := h.schema(), h.policy()
	require.ErrorContains(t, h.down("0.1.290-casbin-down.sql"), "restore the backup")
	require.Equal(t, schema, h.schema())
	require.Equal(t, policy, h.policy())
}

// Keys and rows that would read as reserved policy values are refused: a
// role key _self (subject r:_self, the virtual role every active user holds
// in casbinx), a permission key * (the action of a superuser row), a
// superuser role but super_admin (the one-holder index names it), and a
// row of another shape than the core reads. Rows written before the
// migration stop the upgrade, saying which and how to fix it.
func TestCasbinRuleReservedKeys(t *testing.T) {
	h, _, root := casbinService(t)
	refused := func(sql, constraint string, args ...any) {
		t.Helper()
		_, err := h.db.SQL.ExecContext(h.ctx, sql, args...)
		require.ErrorContains(t, err, constraint)
	}
	refused(`INSERT INTO roles (key, name) VALUES ('_self', '{}')`, "roles_key_reserved")
	refused(`UPDATE roles SET key = '_self' WHERE key = 'user'`, "roles_key_reserved")
	refused(`INSERT INTO permissions (key, module, label, source) VALUES ('*', 'x', '{}', 'core')`, "permissions_key_reserved")
	refused(`INSERT INTO roles (key, name, superuser) VALUES ('root2', '{}', true)`, "roles_superuser_key")
	refused(`UPDATE roles SET superuser = true WHERE key = 'admin'`, "roles_superuser_key")
	h.exec(`INSERT INTO roles (key, name) VALUES ('self_service', '{}')`) // only the whole subject is reserved
	h.exec(`DELETE FROM roles WHERE key = 'self_service'`)
	for _, row := range [][4]string{
		{"p", "r:_self", "account:read", "*"},          // the virtual role
		{"p", "u:1", "account:read", "*"},              // a user's grant
		{"p", "r:user", "account:read", ""},            // no scope
		{"p", "r:user", "*", "own"},                    // a scoped superuser row
		{"g", "u:x", "r:user", ""},                     // not a user id
		{"g", casbinrule.UserSub(root), "r:_self", ""}, // the virtual role
		{"g", casbinrule.UserSub(root), "r:user", "own"},
		{"x", "r:user", "account:read", "*"},
	} {
		refused(`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ($1, $2, $3, $4)`, "casbin_rule_shape", row[0], row[1], row[2], row[3])
	}

	for _, c := range []struct{ seed, want string }{
		{`INSERT INTO roles (key, name) VALUES ('_self', '{}')`, "roles.key _self is reserved"},
		{`INSERT INTO permissions (key, module, label, source) VALUES ('*', 'x', '{}', 'core')`, "permissions.key * is reserved"},
		{`INSERT INTO roles (key, name, superuser) VALUES ('root2', '{}', true)`, "only the role super_admin may be a superuser role, not root2"},
	} {
		old := &hardeningDB{t: t, db: testutil.DBThrough(t, beforeCasbin), ctx: context.Background()}
		old.exec(c.seed)
		require.ErrorContains(t, old.migrate(), c.want)
		require.Equal(t, "false true", old.text(`SELECT (to_regclass('casbin_rule') IS NOT NULL)::text || ' ' || (to_regclass('user_roles') IS NOT NULL)::text`), "the migration rolled back")
	}
}

// Several holders of the super admin role (0097's trigger bypassed by a
// repair) stop the upgrade before the one-holder index would, naming them
// and the CLI that keeps one.
func TestCasbinRuleSeveralSuperAdmins(t *testing.T) {
	h := &hardeningDB{t: t, db: testutil.DBThrough(t, beforeCasbin), ctx: context.Background()}
	h.exec(`INSERT INTO roles (key, name, superuser) VALUES ('super_admin', '{}', true)`)
	a, b := testutil.InsertUser(t, h.db, "a@x"), testutil.InsertUser(t, h.db, "b@x")
	testutil.ExecWithoutTrigger(t, h.db, "user_roles", "user_roles_one_superuser",
		`INSERT INTO user_roles (user_id, role_id) SELECT u, r.id FROM unnest($1::bigint[]) u, roles r WHERE r.key = 'super_admin'`, []int64{a, b})
	err := h.migrate()
	require.ErrorContains(t, err, fmt.Sprintf("several super admins: user ids %d, %d", a, b))
	require.ErrorContains(t, err, `hint: keep one with "sub2api admin recover-superadmin --email`)
	require.Equal(t, "false true", h.text(`SELECT (to_regclass('casbin_rule') IS NOT NULL)::text || ' ' || (to_regclass('user_roles') IS NOT NULL)::text`), "the migration rolled back")
}

// The one-holder index across transactions: of two giving the super admin
// role to different users, the second waits at its insert for the first
// and, once that commits, fails with unique_violation.
func TestCasbinRuleOneSuperuserConcurrent(t *testing.T) {
	db := testutil.DB(t)
	ctx := t.Context()
	_, err := db.SQL.ExecContext(ctx, `INSERT INTO roles (key, name, builtin, superuser) VALUES ('super_admin', '{}', true, true)`)
	require.NoError(t, err)
	a, b := testutil.InsertUser(t, db, "a@su"), testutil.InsertUser(t, db, "b@su")

	first, err := testutil.Begin(ctx, db)
	require.NoError(t, err)
	require.NoError(t, casbinrule.AddMembers(first, authz.RoleSuperAdmin, []int64{a}))
	second, err := testutil.Begin(ctx, db)
	require.NoError(t, err)
	defer second.Rollback()
	errs := make(chan error, 1)
	go func() { errs <- casbinrule.AddMembers(second, authz.RoleSuperAdmin, []int64{b}) }()
	require.Eventually(t, func() bool {
		var n int
		return db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE a.datname = current_database() AND l.locktype = 'transactionid' AND NOT l.granted`).Scan(&n) == nil && n > 0
	}, 10*time.Second, 20*time.Millisecond, "the second insert waits for the first transaction")
	require.NoError(t, first.Commit().Error)
	err = <-errs
	require.True(t, store.IsUniqueViolation(err, "casbin_rule_one_superuser"), "the second holder: %v", err)
}

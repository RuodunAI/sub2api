package migrations_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// effectivePermissions runs the query of deploy/rollback's
// 0.1.290-effective-permissions.sql for the schema at hand (the script
// picks it with psql's \if) and returns its lines as psql prints them.
func (h *hardeningDB) effectivePermissions(casbin bool) string {
	h.t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "rollback", "0.1.290-effective-permissions.sql"))
	require.NoError(h.t, err)
	_, rest, ok := strings.Cut(string(script), "\\if :casbin\n")
	require.True(h.t, ok)
	after, rest, ok := strings.Cut(rest, "\\else\n")
	require.True(h.t, ok)
	before, _, ok := strings.Cut(rest, "\\endif")
	require.True(h.t, ok)
	query := before
	if casbin {
		query = after
	}
	rows, err := h.db.SQL.QueryContext(h.ctx, query)
	require.NoError(h.t, err)
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var uid, key, scope string
		require.NoError(h.t, rows.Scan(&uid, &key, &scope))
		lines = append(lines, uid+"\t"+key+"\t"+scope)
	}
	require.NoError(h.t, rows.Err())
	return strings.Join(lines, "\n")
}

// The rehearsal's comparison (0.1.290-release.md): on a database as
// 0.1.288 leaves it, the script's role-table query lists what users may do
// - the super admin's *, grants over every row and scoped ones, only for
// active users and active permissions - and after 0100 its casbin_rule
// query lists the same lines.
func TestEffectivePermissionsScript(t *testing.T) {
	h := &hardeningDB{t: t, db: testutil.DBThrough(t, "0098_plugin_staged_grants.sql"), ctx: t.Context()}
	h.exec(`
INSERT INTO roles (key, name, builtin, superuser) VALUES ('super_admin', '{}', true, true), ('user', '{}', true, false), ('editor', '{}', false, false);
INSERT INTO permissions (key, module, label, source, status) VALUES
    ('gateway:use', 'gateway', '{}', 'core', 'active'), ('account:read', 'account', '{}', 'core', 'active'), ('core:gone', 'core', '{}', 'core', 'removed');
INSERT INTO users (id, email, password_hash, status, deleted_at) VALUES
    (1, 'root@x', '', 'active', NULL), (2, 'ed@x', '', 'active', NULL), (3, 'off@x', '', 'disabled', NULL), (4, 'gone@x', '', 'active', now());
INSERT INTO role_permissions (role_id, permission_id) SELECT r.id, p.id FROM roles r JOIN permissions p ON (r.key, p.key) IN (
    ('user', 'gateway:use'), ('editor', 'core:gone'), ('editor', 'gateway:use'));
INSERT INTO role_permission_scopes (role_id, permission_id, scope) SELECT r.id, p.id, s FROM roles r JOIN permissions p ON p.key = 'account:read'
    CROSS JOIN (VALUES ('own'), ('group/12/account/5')) v(s) WHERE r.key = 'editor';
INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u JOIN roles r ON (u.email, r.key) IN (
    ('root@x', 'super_admin'), ('ed@x', 'editor'), ('ed@x', 'user'), ('off@x', 'user'), ('gone@x', 'editor'))`)

	before := h.effectivePermissions(false)
	require.Equal(t, strings.Join([]string{
		"1\t*\t*",
		"2\taccount:read\tgroup/12/account/5", "2\taccount:read\town", "2\tgateway:use\t*",
	}, "\n"), before)
	require.NoError(t, h.migrate())
	require.Equal(t, before, h.effectivePermissions(true))
}

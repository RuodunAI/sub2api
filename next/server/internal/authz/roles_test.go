package authz

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// rolePermissionKeys is every grant row of a role, removed permissions
// included, sorted.
func rolePermissionKeys(t *testing.T, s *Service, id int64) []string {
	t.Helper()
	var keys []string
	if err := s.db.Pool.QueryRow(context.Background(), `SELECT COALESCE(array_agg(p.key ORDER BY p.key), '{}')
FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE rp.role_id = $1`, id).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	return keys
}

// Nobody, the super admin and the system included, changes the super admin
// role in any way.
func TestSuperuserRoleFrozen(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	suID := roleID(t, s, RoleSuperAdmin)

	for _, actor := range []int64{root, admin, 0} {
		writes := map[string]error{}
		_, writes["name"] = s.UpdateRole(ctx, actor, suID, UpdateRoleInput{Name: lt("Root", "根")})
		_, writes["description"] = s.UpdateRole(ctx, actor, suID, UpdateRoleInput{Description: lt("x", "x")})
		_, writes["permissions"] = s.SetRolePermissions(ctx, actor, suID, []string{"user:read"}, nil)
		_, writes["reset"] = s.ResetRole(ctx, actor, suID)
		writes["delete"] = s.DeleteRole(ctx, actor, suID)
		for op, err := range writes {
			if !isCode(err, "conflict") {
				t.Errorf("actor %d %s super_admin: %v", actor, op, err)
			}
		}
	}
	if r, err := s.GetRole(ctx, suID); err != nil || r.Name.Get("zh") != "超级管理员" {
		t.Fatalf("super_admin changed: %+v %v", r, err)
	}
}

// A built-in role other than the super admin's is changed (renamed, its
// permissions or assignable list replaced, reset) by whoever has it on the
// assignable list and holds what it grants, as with a custom role holding
// users (editableRole): with the default lists the admin changes operator,
// supplier and user, never admin; nobody deletes one or changes its key.
// What is added must still be held (cannotGrant). An access key has no
// assignable list, the super admin's included.
func TestBuiltinRolesEditable(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	operator := addUser(t, s, "op@x.com", RoleOperator)

	writes := func(actor, id int64, perms []string) map[string]error {
		out := map[string]error{}
		_, out["rename"] = s.UpdateRole(ctx, actor, id, UpdateRoleInput{Name: lt("x", "x")})
		_, out["permissions"] = s.SetRolePermissions(ctx, actor, id, perms, nil)
		_, out["assignable"] = s.SetRoleAssignable(ctx, actor, id, AssignableInput{})
		_, out["reset"] = s.ResetRole(ctx, actor, id)
		return out
	}
	// Not on the actor's list: the admin's own role, the operator's own.
	for _, c := range []struct {
		actor int64
		key   string
	}{{admin, RoleAdmin}, {operator, RoleOperator}, {operator, RoleAdmin}} {
		id := roleID(t, s, c.key)
		before, err := s.GetRole(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		denied := writes(c.actor, id, []string{"user:read"})
		denied["delete"] = s.DeleteRole(ctx, c.actor, id)
		for op, err := range denied {
			if !isCode(err, "permission_denied") {
				t.Errorf("actor %d %s %s: %v", c.actor, op, c.key, err)
			}
		}
		after, err := s.GetRole(ctx, id)
		if err != nil || !slices.Equal(after.PermissionKeys, before.PermissionKeys) || after.Name.Get("en") != before.Name.Get("en") {
			t.Fatalf("%s changed: %+v, %v", c.key, after, err)
		}
	}

	// On the admin's list and within it: changed, reset; never deleted, the
	// key unchanged.
	for _, key := range []string{RoleOperator, RoleSupplier, RoleUser} {
		id := roleID(t, s, key)
		def, err := s.GetRole(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for op, err := range writes(admin, id, []string{"user:read", "balance:adjust"}) {
			if err != nil {
				t.Errorf("admin %s %s: %v", op, key, err)
			}
		}
		if _, err := s.SetRolePermissions(ctx, admin, id, []string{"user:read"}, nil); err != nil {
			t.Fatal(err)
		}
		r, err := s.ResetRole(ctx, admin, id)
		if err != nil || r.Key != key || !slices.Equal(r.PermissionKeys, def.PermissionKeys) || !slices.Equal(r.AssignableRoleKeys, def.AssignableRoleKeys) ||
			r.Name.Get("en") != def.Name.Get("en") {
			t.Fatalf("admin resets %s: %+v %v\nwant %+v", key, r, err, def)
		}
		if err := s.DeleteRole(ctx, admin, id); !isCode(err, "conflict") {
			t.Fatalf("admin deletes %s: %v", key, err)
		}
	}

	opID := roleID(t, s, RoleOperator)
	// Adding what the admin lacks is a grant beyond it.
	cur := rolePermissionKeys(t, s, opID)
	if _, err := s.SetRolePermissions(ctx, admin, opID, append(cur, "publisher:manage"), nil); !isCode(err, "permission_denied") || detail(err, "missing") == nil {
		t.Fatalf("admin grants publisher:manage to operator: %v", err)
	}
	// The super admin's access key is refused: it has no assignable list.
	keyed := core.WithAccessKey(ctx, core.AccessKeyIdentity{UserID: root, Permissions: []string{"role:define", "user:read"}})
	if _, err := s.SetRolePermissions(keyed, root, opID, []string{"user:read"}, nil); !isCode(err, "permission_denied") || detail(err, "missing_assignable") == nil {
		t.Fatalf("super admin's access key changes operator: %v", err)
	}
	// Once the super admin grants the operator more than the admin holds,
	// the admin can no longer change or reset it (M5).
	if _, err := s.SetRolePermissions(ctx, root, opID, append(cur, "publisher:manage"), nil); err != nil {
		t.Fatal(err)
	}
	for op, err := range writes(admin, opID, cur) {
		if !isCode(err, "permission_denied") {
			t.Errorf("admin %s an operator above it: %v", op, err)
		}
	}

	// The super admin and the system change and reset every one of them,
	// admin included; deleting stays refused for everyone (built-in).
	for _, actor := range []int64{root, 0} {
		for _, key := range []string{RoleAdmin, RoleOperator, RoleSupplier, RoleUser} {
			for op, err := range writes(actor, roleID(t, s, key), []string{"user:read"}) {
				if err != nil {
					t.Errorf("actor %d %s %s: %v", actor, op, key, err)
				}
			}
		}
	}
	if err := s.DeleteRole(ctx, root, opID); !isCode(err, "conflict") {
		t.Fatalf("super admin deletes operator: %v", err)
	}
}

// A reset hands out every default again, so the actor must hold them all:
// a definer that may change the user role (within it, on its list) but
// lacks a default of it is refused with what it misses.
func TestResetNeedsEveryDefault(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	if _, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "keeper", Name: lt("Keeper", "管家"),
		PermissionKeys: []string{"role:define", "gateway:use"}, AssignableRoleKeys: []string{RoleUser}}); err != nil {
		t.Fatal(err)
	}
	keeper := addUser(t, s, "keeper@x.com", "keeper")
	userID := roleID(t, s, RoleUser)
	if _, err := s.SetRolePermissions(ctx, root, userID, []string{"gateway:use"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRole(ctx, keeper, userID, UpdateRoleInput{Description: lt("Gateway only", "只能调网关")}); err != nil {
		t.Fatalf("keeper edits the user role: %v", err)
	}
	_, err := s.ResetRole(ctx, keeper, userID)
	missing, _ := detail(err, "missing").([]string)
	if !isCode(err, "permission_denied") || !slices.Contains(missing, "apikey:self:manage") {
		t.Fatalf("keeper resets the user role: %v (missing %v)", err, detail(err, "missing"))
	}
	if got := rolePermissionKeys(t, s, userID); !slices.Equal(got, []string{"gateway:use"}) {
		t.Fatalf("user role after a refused reset: %v", got)
	}
}

// Decision D2: the super-only permissions are in no built-in role's defaults,
// on a new database or after a reset; a sync neither grants them nor takes
// away what a role already holds; and the super admin, who alone holds them
// by default, is the one to hand them to a role.
func TestSuperOnlyPermissions(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	superOnly := superOnlyKeys()
	holds := func(roleKey string) []string {
		t.Helper()
		var held []string
		for _, k := range rolePermissionKeys(t, s, roleID(t, s, roleKey)) {
			if slices.Contains(superOnly, k) {
				held = append(held, k)
			}
		}
		return held
	}
	for _, b := range builtinRoles {
		if got := holds(b.key); len(got) != 0 {
			t.Errorf("%s holds %v by default", b.key, got)
		}
	}
	for _, k := range superOnly {
		mustCan(t, s, admin, k, false)
		mustCan(t, s, root, k, true)
	}

	// The admin cannot hand them out; the super admin can, to a custom role
	// and, explicitly, to a built-in one.
	if _, err := s.CreateRole(ctx, admin, CreateRoleInput{Key: "updater", Name: lt("Updater", "升级"), PermissionKeys: []string{"system:update:execute"}}); !isCode(err, "permission_denied") {
		t.Fatalf("admin grants a super-only permission: %v", err)
	}
	if _, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "updater", Name: lt("Updater", "升级"), PermissionKeys: superOnly}); err != nil {
		t.Fatalf("super admin grants super-only permissions: %v", err)
	}
	if got := holds("updater"); len(got) != len(superOnly) {
		t.Fatalf("updater holds %v", got)
	}
	adminID := roleID(t, s, RoleAdmin)
	cur := rolePermissionKeys(t, s, adminID)
	if _, err := s.SetRolePermissions(ctx, root, adminID, append(cur, "publisher:manage"), nil); err != nil {
		t.Fatal(err)
	}
	mustCan(t, s, admin, "publisher:manage", true)

	// A sync leaves what the role holds; a reset takes it back to defaults.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM permissions WHERE key = 'system:update:recover'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncCore(ctx); err != nil {
		t.Fatal(err)
	}
	if got := holds(RoleAdmin); !slices.Equal(got, []string{"publisher:manage"}) {
		t.Fatalf("admin after a sync that created a super-only permission: %v", got)
	}
	if _, err := s.ResetRole(ctx, root, adminID); err != nil {
		t.Fatal(err)
	}
	if got := holds(RoleAdmin); len(got) != 0 {
		t.Fatalf("admin after a reset: %v", got)
	}
	mustCan(t, s, admin, "publisher:manage", false)
}

// The operator whitelist (decision D3) is what a reset restores; a sync
// leaves an existing operator's permissions as they are, moving audit:read
// to its own module without touching who holds it.
func TestOperatorWhitelist(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	opID := roleID(t, s, RoleOperator)

	// The operator as an older version left it: beyond the whitelist here,
	// short of it there, and audit:read still in the cluster module.
	before := []string{"user:read", "user:delete", "role:define", "balance:adjust", "audit:read", "price:manage"}
	if _, err := s.SetRolePermissions(ctx, root, opID, before, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE permissions SET module = 'node' WHERE key = 'audit:read'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncCore(ctx); err != nil {
		t.Fatal(err)
	}
	slices.Sort(before)
	if got := rolePermissionKeys(t, s, opID); !slices.Equal(got, before) {
		t.Fatalf("a sync changed the operator: %v, want %v", got, before)
	}
	var module string
	if err := db.Pool.QueryRow(ctx, `SELECT module FROM permissions WHERE key = 'audit:read'`).Scan(&module); err != nil || module != "audit" {
		t.Fatalf("audit:read module = %q, %v", module, err)
	}

	r, err := s.ResetRole(ctx, root, opID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.PermissionKeys, operatorDefaults) {
		t.Fatalf("reset operator = %v\nwant %v", r.PermissionKeys, operatorDefaults)
	}
	// What the role is for, and what it is kept from.
	operator := addUser(t, s, "op@x.com", RoleOperator)
	for key, want := range map[string]bool{
		"account:update": true, "role:assign": true, "audit:read": true, "task:run": true, "account:credential:view:own": true,
		"role:define": false, "balance:adjust": false, "usage:settle": false, "user:delete": false, "user:password:reset": false,
		"account:credential:view": false, "account:relay": false, "system:update:execute": false, "settings:read": false, "plugin:read": false,
	} {
		mustCan(t, s, operator, key, want)
	}
	// It can hand out the supplier role, which its assignable list names.
	plain := addUser(t, s, "plain@x.com", RoleUser)
	if err := setRoles(s, operator, plain, RoleUser, RoleSupplier); err != nil {
		t.Fatalf("operator assigns supplier: %v", err)
	}
	// ...and still act on the supplier afterwards, to take the role back.
	if err := s.CanActOnUser(ctx, operator, core.ActOnIdentity, plain); err != nil {
		t.Fatalf("operator acts on a supplier: %v", err)
	}
	if err := setRoles(s, operator, plain, RoleUser); err != nil {
		t.Fatalf("operator revokes supplier: %v", err)
	}
}

// M5: a role:define holder cannot change or delete a role granting more than
// it holds, so it cannot weaken a stronger user's role and then act on that
// user. Roles within its own permissions stay editable.
func TestRoleWritesNeedActOnRole(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	for key, perms := range map[string][]string{
		"helper": {"role:define", "user:read", "user:password:reset"},
		"big":    {"user:read", "settings:manage"},
		"small":  {"user:read"},
	} {
		if _, err := s.CreateRole(ctx, root, CreateRoleInput{Key: key, Name: lt(key, key), PermissionKeys: perms}); err != nil {
			t.Fatal(err)
		}
	}
	helper := addUser(t, s, "helper@x.com", "helper")
	victim := addUser(t, s, "victim@x.com", "big")
	bigID, smallID := roleID(t, s, "big"), roleID(t, s, "small")

	writes := map[string]error{}
	_, writes["weaken big"] = s.SetRolePermissions(ctx, helper, bigID, []string{"user:read"}, nil)
	_, writes["rename big"] = s.UpdateRole(ctx, helper, bigID, UpdateRoleInput{Name: lt("x", "x")})
	writes["delete big"] = s.DeleteRole(ctx, helper, bigID)
	_, writes["reset admin"] = s.ResetRole(ctx, helper, roleID(t, s, RoleAdmin))
	for op, err := range writes {
		if !isCode(err, "permission_denied") {
			t.Errorf("helper may %s: %v", op, err)
		}
	}
	if got := rolePermissionKeys(t, s, bigID); !slices.Equal(got, []string{"settings:manage", "user:read"}) {
		t.Fatalf("big changed: %v", got)
	}
	if err := s.CanActOnUser(ctx, helper, core.ActOnIdentity, victim); !isCode(err, "permission_denied") {
		t.Fatalf("helper acts on victim: %v", err)
	}

	// Within its own permissions: it may add what it holds, nothing more.
	if _, err := s.SetRolePermissions(ctx, helper, smallID, []string{"user:read", "user:password:reset"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRolePermissions(ctx, helper, smallID, []string{"user:read", "settings:manage"}, nil); !isCode(err, "permission_denied") {
		t.Fatalf("helper grants settings:manage: %v", err)
	}
	if err := s.DeleteRole(ctx, helper, smallID); err != nil {
		t.Fatal(err)
	}
}

// Saving a role keeps grants of disabled permissions and the removed ones
// it lists (a role saves back as listed, P2), drops removed ones it leaves
// out (they can be unticked) and never adds a removed one it does not hold
// (nor can they be ticked). A reset keeps removed core grants.
func TestSetRolePermissionsKeepsInactive(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	_, err := db.Pool.Exec(ctx, `INSERT INTO plugins (key, name, status) VALUES ('guard', '{"en":"Guard"}', 'installed')`)
	require.NoError(t, err)
	require.NoError(t, db.Tx(ctx, func(tx pgx.Tx) error {
		return s.SyncPlugin(ctx, tx, "guard", withRoles([]core.PermissionDef{{Key: "stats:read"}, {Key: "audit:read"}}, RoleAdmin))
	}))
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "watch", Name: lt("Watch", "watch"),
		PermissionKeys: []string{"account:read", "proxy:read", "plugin.guard:stats:read"}})
	require.NoError(t, err)
	require.NoError(t, db.Tx(ctx, func(tx pgx.Tx) error { return s.SetPluginActive(ctx, tx, "guard", false) }))
	_, err = db.Pool.Exec(ctx, `UPDATE permissions SET status = 'removed' WHERE key IN ('proxy:read', 'plugin.guard:audit:read')`)
	require.NoError(t, err)
	held := []string{"account:read", "plugin.guard:stats:read", "proxy:read"}

	_, err = s.SetRolePermissions(ctx, admin, r.ID, r.PermissionKeys, nil)
	require.NoError(t, err, "save as listed")
	assert.Equal(t, held, rolePermissionKeys(t, s, r.ID))
	_, err = s.SetRolePermissions(ctx, admin, r.ID, append(slices.Clone(r.PermissionKeys), "plugin.guard:audit:read"), nil)
	require.NoError(t, err, "a removed key the role does not hold is ignored")
	assert.Equal(t, held, rolePermissionKeys(t, s, r.ID))
	_, err = s.SetRolePermissions(ctx, admin, r.ID, []string{"account:read", "plugin.guard:stats:read"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"account:read", "plugin.guard:stats:read"}, rolePermissionKeys(t, s, r.ID), "the removed one left out goes")
	_, err = s.SetRolePermissions(ctx, admin, r.ID, []string{"nope:x"}, nil)
	assert.True(t, isCode(err, "invalid_argument"), "unknown key: %v", err)

	op := roleID(t, s, RoleOperator)
	require.Contains(t, rolePermissionKeys(t, s, op), "proxy:read")
	_, err = s.ResetRole(ctx, root, op)
	require.NoError(t, err)
	assert.Contains(t, rolePermissionKeys(t, s, op), "proxy:read", "a reset keeps removed core grants")
}

// A plugin's role matrix: GET shows every role's grants of the plugin and
// whether the caller may change the role; PUT replaces the listed roles'
// grants of that plugin only, each as a role write (editableRole, covers),
// all or none, a refusal naming its role.
func TestPluginRoleGrantsMatrix(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	flow := &manifest.PermissionScope{Resource: "flow", Own: true}
	require.NoError(t, syncer(t, s, "m")(
		core.PermissionDef{Key: "flow:read", GrantRoles: unscoped(RoleAdmin)},
		core.PermissionDef{Key: "flow:manage", ScopeType: "plugin:flow", Scope: flow, GrantRoles: unscoped(RoleAdmin)},
		core.PermissionDef{Key: "flow:delete"}))
	require.NoError(t, syncer(t, s, "other")(core.PermissionDef{Key: "x:read", GrantRoles: unscoped(RoleAdmin)}))
	op, user := roleID(t, s, RoleOperator), roleID(t, s, RoleUser)
	opBefore, userBefore := rolePermissionKeys(t, s, op), rolePermissionKeys(t, s, user)

	m, err := s.PluginRoleGrants(ctx, admin, "m")
	require.NoError(t, err)
	assert.Equal(t, "plugin.m", m.Module.Module)
	rows := map[string]PluginRoleRow{}
	for _, r := range m.Roles {
		rows[r.Key] = r
	}
	assert.True(t, !rows[RoleAdmin].Editable && rows[RoleAdmin].Denied == core.GrantDenyNotAssignable, "%+v", rows[RoleAdmin])
	assert.ElementsMatch(t, []string{"plugin.m:flow:read", "plugin.m:flow:manage"}, rows[RoleAdmin].PermissionKeys)
	assert.True(t, rows[RoleSuperAdmin].Superuser && !rows[RoleSuperAdmin].Editable && rows[RoleSuperAdmin].Denied == core.GrantDenySuperuserRole, "%+v", rows[RoleSuperAdmin])
	assert.True(t, rows[RoleOperator].Editable && rows[RoleOperator].Denied == "", "%+v", rows[RoleOperator])
	assert.Equal(t, []string{}, rows[RoleOperator].PermissionKeys)

	// The second role fails: nothing is written, and the refusal names it.
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read"}, RoleUser: {"plugin.m:flow:delete"}}, nil)
	var ce *core.Error
	require.True(t, errors.As(err, &ce) && ce.Code == "permission_denied", "%v", err)
	assert.Equal(t, RoleUser, ce.Details["role"])
	assert.Equal(t, []string{"plugin.m:flow:delete"}, ce.Details["missing"])
	assert.Equal(t, opBefore, rolePermissionKeys(t, s, op), "rolled back")
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleOperator: {"plugin.other:x:read"}}, nil)
	assert.Equal(t, "foreign", fieldCode(err, "roles.operator"), "%v", err)
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{"nope": {}}, nil)
	assert.Equal(t, "unknown", fieldCode(err, "roles.nope"), "%v", err)
	_, err = s.SetPluginRoleGrants(ctx, admin, "nope", map[string][]string{RoleOperator: {}}, nil)
	assert.True(t, isCode(err, "not_found"), "%v", err)
	_, err = s.PluginRoleGrants(ctx, admin, "nope")
	assert.True(t, isCode(err, "not_found"), "%v", err)

	// Other roles and the role's other grants stay.
	set := []string{"plugin.m:flow:read", "plugin.m:flow:manage:flow/2", "plugin.m:flow:manage:flow/5"}
	m, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleOperator: set}, nil)
	require.NoError(t, err)
	i := slices.IndexFunc(m.Roles, func(r PluginRoleRow) bool { return r.Key == RoleOperator })
	assert.ElementsMatch(t, set, m.Roles[i].PermissionKeys)
	assert.ElementsMatch(t, append(slices.Clone(opBefore), "plugin.m:flow:read"), rolePermissionKeys(t, s, op))
	assert.Equal(t, userBefore, rolePermissionKeys(t, s, user))
	var via string
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT detail->>'via' FROM audit_logs WHERE action = 'role.permissions' AND target_id::bigint = $1`, op).Scan(&via))
	assert.Equal(t, "plugin_matrix", via)
	_, err = s.SetPluginRoleGrants(ctx, admin, "m", map[string][]string{RoleOperator: {}}, nil)
	require.NoError(t, err)
	assert.Equal(t, opBefore, rolePermissionKeys(t, s, op))
	assert.Empty(t, scopedHolders(t, db, "plugin.m:flow:manage"))
}

// Every role write leaves one audit row, by its actor, on the role.
func TestRoleWritesAudited(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "ops", Name: lt("Ops", "运营"), PermissionKeys: []string{"user:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRole(ctx, root, r.ID, UpdateRoleInput{Name: lt("Ops 2", "运营 2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRolePermissions(ctx, root, r.ID, []string{"user:read", "account:read"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRole(ctx, root, r.ID); err != nil {
		t.Fatal(err)
	}
	opID := roleID(t, s, RoleOperator)
	if _, err := s.ResetRole(ctx, root, opID); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Pool.Query(ctx, `SELECT action, target_id::bigint FROM audit_logs
WHERE target_type = 'role' AND action LIKE 'role.%' AND user_id = $1 ORDER BY id`, root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var action string
		var id int64
		if err := rows.Scan(&action, &id); err != nil {
			t.Fatal(err)
		}
		want := r.ID
		if action == "role.reset" {
			want = opID
		}
		if id != want {
			t.Errorf("%s on role %d, want %d", action, id, want)
		}
		got = append(got, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"role.create", "role.update", "role.permissions", "role.delete", "role.reset"}
	if !slices.Equal(got, want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	// The reset says what the role had and now has.
	var assign string
	if err := db.Pool.QueryRow(ctx, `SELECT detail->'after'->>'assignable_role_keys' FROM audit_logs WHERE action = 'role.reset' AND detail->>'key' = 'operator'
  AND detail->'after'->'permission_keys' ? 'task:run' AND detail->'before'->'permission_keys' ? 'task:run' AND NOT detail ? 'via'`).Scan(&assign); err != nil || assign != `["supplier", "user"]` {
		t.Fatalf("role.reset detail: %q %v", assign, err)
	}
}

// A role's key never changes: PATCH /roles/:id refuses a different key with
// a field error on key (built-in roles included) and ignores the same one.
func TestUpdateRoleKeyImmutable(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	r, err := s.CreateRole(ctx, root, CreateRoleInput{Key: "ops", Name: lt("Ops", "运营")})
	if err != nil {
		t.Fatal(err)
	}
	key := func(k string) *string { return &k }

	for _, id := range []int64{r.ID, roleID(t, s, RoleOperator)} {
		_, err := s.UpdateRole(ctx, root, id, UpdateRoleInput{Key: key("renamed"), Name: lt("Renamed", "改名")})
		if !isCode(err, "invalid_argument") || fieldCode(err, "key") != "immutable" {
			t.Fatalf("role %d: different key: %v", id, err)
		}
		if got, _ := s.GetRole(ctx, id); got.Name.Get("en") == "Renamed" {
			t.Fatalf("role %d renamed by a refused update", id)
		}
	}

	// The refusal speaks the request's language.
	_, err = s.UpdateRole(core.WithLocale(ctx, "zh"), root, r.ID, UpdateRoleInput{Key: key("renamed")})
	if !isCode(err, "invalid_argument") || !strings.Contains(err.Error(), "角色标识创建后不可修改") {
		t.Fatalf("zh refusal: %v", err)
	}

	got, err := s.UpdateRole(ctx, root, r.ID, UpdateRoleInput{Key: key("ops"), Name: lt("Ops 2", "运营 2")})
	if err != nil {
		t.Fatalf("same key: %v", err)
	}
	if got.Key != "ops" || got.Name.Get("zh") != "运营 2" {
		t.Fatalf("same key: got %s %v", got.Key, got.Name)
	}
}

// A role read and a plugin matrix carry the authz version they were read
// at; a permissions write naming another version is refused with
// version_conflict and writes nothing, one naming the current version or
// none goes through (CONTRACTS C1).
func TestPermissionWritesCheckVersion(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	require.NoError(t, syncer(t, s, "m")(core.PermissionDef{Key: "flow:read"}))
	op := roleID(t, s, RoleOperator)

	r, err := s.GetRole(ctx, op)
	require.NoError(t, err)
	m, err := s.PluginRoleGrants(ctx, root, "m")
	require.NoError(t, err)
	assert.Equal(t, r.Version, m.Version)
	// Someone else's write moves the version.
	_, err = s.SetRolePermissions(ctx, root, roleID(t, s, RoleUser), rolePermissionKeys(t, s, roleID(t, s, RoleUser)), nil)
	require.NoError(t, err)

	stale := r.Version
	_, err = s.SetRolePermissions(ctx, root, op, append(slices.Clone(r.PermissionKeys), "plugin.m:flow:read"), &stale)
	require.True(t, isCode(err, "version_conflict"), "%v", err)
	assert.Equal(t, stale+1, core.AsError(err).Details["version"])
	assert.Equal(t, http.StatusConflict, core.AsError(err).Status)
	_, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read"}}, &stale)
	require.True(t, isCode(err, "version_conflict"), "%v", err)
	assert.NotContains(t, rolePermissionKeys(t, s, op), "plugin.m:flow:read")

	m, err = s.PluginRoleGrants(ctx, root, "m")
	require.NoError(t, err)
	m, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read"}}, &m.Version)
	require.NoError(t, err)
	r, err = s.SetRolePermissions(ctx, root, op, rolePermissionKeys(t, s, op), &m.Version)
	require.NoError(t, err)
	assert.Equal(t, m.Version+1, r.Version)
}

// stepUp is a step-up verifier whose proof is valid when ok, counting the
// proofs asked for.
type stepUp struct {
	ok    bool
	asked int
}

func (v *stepUp) ConsumeStepUp(_ context.Context, scope string) error {
	v.asked++
	if v.ok && scope == core.StepUpPluginGrant {
		return nil
	}
	return core.ErrVerificationRequired.WithDetails(map[string]any{"scope": scope})
}

// Handing a role a critical plugin permission, through the matrix, the
// role's permissions or a new role, takes a plugin.grant step-up proof
// (CONTRACTS C3), as a consent granting it does; keeping one, or adding
// anything else, takes none.
func TestCriticalPluginGrantNeedsStepUp(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	require.NoError(t, syncer(t, s, "m")(
		core.PermissionDef{Key: "flow:read", Risk: manifest.RiskSensitive},
		core.PermissionDef{Key: "flow:purge", Risk: manifest.RiskCritical}))
	v := &stepUp{}
	s.UseStepUp(v)
	op := roleID(t, s, RoleOperator)
	purge := "plugin.m:flow:purge"

	_, err := s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {purge}}, nil)
	require.True(t, isCode(err, "verification_required"), "%v", err)
	assert.Equal(t, core.StepUpPluginGrant, core.AsError(err).Details["scope"])
	_, err = s.SetRolePermissions(ctx, root, op, append(rolePermissionKeys(t, s, op), purge), nil)
	require.True(t, isCode(err, "verification_required"), "%v", err)
	_, err = s.CreateRole(ctx, root, CreateRoleInput{Key: "purger", Name: lt("Purger", "清理"), PermissionKeys: []string{purge}})
	require.True(t, isCode(err, "verification_required"), "%v", err)
	assert.NotContains(t, rolePermissionKeys(t, s, op), purge)

	_, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read"}}, nil)
	require.NoError(t, err, "a sensitive one takes no proof")
	v.ok = true
	_, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read", purge}}, nil)
	require.NoError(t, err)
	assert.Contains(t, rolePermissionKeys(t, s, op), purge)
	v.ok, v.asked = false, 0
	_, err = s.SetRolePermissions(ctx, root, op, rolePermissionKeys(t, s, op), nil)
	require.NoError(t, err, "kept, not added")
	assert.Zero(t, v.asked)
}

// A matrix write's audit says what it dropped, the grant of a removed
// permission included, and the plugin's part before and after; a whole
// role write says what it dropped too.
func TestRolePermissionWritesAuditRemovals(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	sync := syncer(t, s, "m")
	require.NoError(t, sync(core.PermissionDef{Key: "flow:read", GrantRoles: unscoped(RoleOperator)},
		core.PermissionDef{Key: "flow:old", GrantRoles: unscoped(RoleOperator)}))
	require.NoError(t, sync(core.PermissionDef{Key: "flow:read"})) // flow:old removed, its grant kept
	op := roleID(t, s, RoleOperator)

	_, err := s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {}}, nil)
	require.NoError(t, err)
	var detail struct {
		PermissionKeys []string `json:"permission_keys"`
		Removed        []string `json:"removed"`
		Before         []string `json:"before"`
		After          []string `json:"after"`
	}
	var raw []byte
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT detail FROM audit_logs WHERE action = 'role.permissions' AND target_id::bigint = $1
ORDER BY id DESC LIMIT 1`, op).Scan(&raw))
	require.NoError(t, json.Unmarshal(raw, &detail))
	assert.ElementsMatch(t, []string{"plugin.m:flow:read", "plugin.m:flow:old"}, detail.Removed)
	assert.ElementsMatch(t, []string{"plugin.m:flow:read", "plugin.m:flow:old"}, detail.Before)
	assert.Equal(t, []string{}, detail.After)
	assert.Equal(t, []string{}, detail.PermissionKeys)

	r, err := s.GetRole(ctx, op)
	require.NoError(t, err)
	keys := r.PermissionKeys
	_, err = s.SetRolePermissions(ctx, root, op, keys[1:], nil)
	require.NoError(t, err)
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT detail FROM audit_logs WHERE action = 'role.permissions' AND target_id::bigint = $1
ORDER BY id DESC LIMIT 1`, op).Scan(&raw))
	detail.Before = nil
	require.NoError(t, json.Unmarshal(raw, &detail))
	assert.Equal(t, keys[:1], detail.Removed)
	assert.Nil(t, detail.Before, "a whole role write lists no part")
}

// A staged permission belongs to a version not activated yet (N3): no
// editor sees it or grants it, and no role write drops what that version's
// consent granted; nor does the grant keep anyone from editing the role.
func TestStagedPermissionsAreNoEditorsToSee(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	const next = "plugin.m:flow:next"
	require.NoError(t, syncer(t, s, "m")(core.PermissionDef{Key: "flow:read"},
		core.PermissionDef{Key: "flow:next", Staged: true, GrantRoles: map[string][]string{RoleOperator: {""}}}))
	op := roleID(t, s, RoleOperator)
	require.Equal(t, []string{RoleOperator}, permissionHolders(t, db, next))

	mods, err := s.ListPermissions(ctx)
	require.NoError(t, err)
	for _, m := range mods {
		for _, p := range m.Permissions {
			assert.NotEqual(t, next, p.Key)
		}
	}
	r, err := s.GetRole(ctx, op)
	require.NoError(t, err)
	assert.NotContains(t, r.PermissionKeys, next)
	m, err := s.PluginRoleGrants(ctx, admin, "m")
	require.NoError(t, err)
	require.Len(t, m.Module.Permissions, 1)
	for _, row := range m.Roles {
		assert.NotContains(t, row.PermissionKeys, next, row.Key)
		if row.Key == RoleOperator {
			assert.True(t, row.Editable, "the staged grant is outside what editing compares")
		}
	}

	_, err = s.SetRolePermissions(ctx, root, op, append(slices.Clone(r.PermissionKeys), next), nil)
	assert.Equal(t, "unknown", fieldCode(err, "permission_keys"), "%v", err)
	_, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleUser: {next}}, nil)
	assert.Equal(t, "unknown", fieldCode(err, "permission_keys"), "%v", err)

	_, err = s.SetRolePermissions(ctx, root, op, r.PermissionKeys, nil)
	require.NoError(t, err)
	_, err = s.SetPluginRoleGrants(ctx, root, "m", map[string][]string{RoleOperator: {"plugin.m:flow:read"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{RoleOperator}, permissionHolders(t, db, next), "the consent's grant stands")
}

package authz

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the authz statements built with GORM (see package gormtest):
// every write, the version and the reads behind each authorization check.

// sqlCase is one snapshot: a subtest name and its statement.
type sqlCase struct {
	name  string
	build func(*gorm.DB) *gorm.DB
}

func assertCases(t *testing.T, cases []sqlCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { gormtest.Assert(t, c.build) })
	}
}

// scan finishes a Raw or chained query for its snapshot.
func scan(q *gorm.DB) *gorm.DB {
	var dest []map[string]any
	return q.Scan(&dest)
}

// The version: Bump locks and increments it (every authz write), the
// readers select it, SyncCore locks it without a bump.
func TestSQLAuthzVersion(t *testing.T) {
	assertCases(t, []sqlCase{
		{"bump", func(db *gorm.DB) *gorm.DB { return scan(bumpVersion(db)) }},
		{"read", func(db *gorm.DB) *gorm.DB { return scan(readVersion(db)) }},
		{"lock", lockVersion},
	})
}

// The reads of an authorization check (loadPermissionSet, ensureCatalog)
// and of the grant view (RolesPermissions, usersRoles, grantOf).
func TestSQLAuthzPermissionLoad(t *testing.T) {
	assertCases(t, []sqlCase{
		{"user_grants", func(db *gorm.DB) *gorm.DB { return scan(userGrants(db, 7)) }},
		{"catalog", func(db *gorm.DB) *gorm.DB { return scan(catalogRows(db)) }},
		{"role_grants", func(db *gorm.DB) *gorm.DB { return scan(roleGrants(db, []string{RoleAdmin, "custom"})) }},
		{"role_assignables", func(db *gorm.DB) *gorm.DB { return scan(roleAssignables(db, []string{RoleAdmin})) }},
		{"custom_roles", func(db *gorm.DB) *gorm.DB { return scan(customRoleKeys(db, "")) }},
		{"custom_roles_but", func(db *gorm.DB) *gorm.DB { return scan(customRoleKeys(db, "custom")) }},
		{"users_role_keys", func(db *gorm.DB) *gorm.DB { return scan(usersRoleKeys(db, []int64{7, 8})) }},
	})
}

// The role reads, the locked one of editableRole included, and the checks
// that guard the super admin.
func TestSQLAuthzRoleReads(t *testing.T) {
	assertCases(t, []sqlCase{
		{"list", func(db *gorm.DB) *gorm.DB { return scan(selectRoles(db, ` ORDER BY r.builtin DESC, r.id`)) }},
		{"lock", func(db *gorm.DB) *gorm.DB { return scan(lockRole(db, 3)) }},
		{"by_keys", func(db *gorm.DB) *gorm.DB {
			return scan(selectRoles(db, ` WHERE r.key = ANY(?) ORDER BY r.key`, store.Strings{RoleAdmin, RoleUser}))
		}},
		{"user_roles", func(db *gorm.DB) *gorm.DB { return scan(db.Raw(userRolesSQL, int64(7))) }},
		{"assignable", func(db *gorm.DB) *gorm.DB { return scan(assignableRoleRows(db)) }},
		{"is_superuser", func(db *gorm.DB) *gorm.DB { return scan(store.ExistsQuery(db, superuserGrants(db, 7))) }},
		{"other_superusers", func(db *gorm.DB) *gorm.DB { return scan(otherSuperusers(db, 7)) }},
		{"lock_super_admins", func(db *gorm.DB) *gorm.DB { return scan(lockSuperAdmins(db)) }},
		{"lock_transfer_target", func(db *gorm.DB) *gorm.DB { return scan(lockTransferTarget(db, 9)) }},
	})
}

func TestSQLAuthzPermissionReads(t *testing.T) {
	assertCases(t, []sqlCase{
		{"grantable", func(db *gorm.DB) *gorm.DB { return scan(grantablePermissions(db, []string{"account:read"})) }},
		{"list", func(db *gorm.DB) *gorm.DB { return scan(permissionRows(db, "demo")) }},
		{"holders", func(db *gorm.DB) *gorm.DB { return scan(holderRows(db, []string{"plugin:demo:read"})) }},
		{"plugin", func(db *gorm.DB) *gorm.DB { return scan(pluginPermissions(db, "demo")) }},
		{"plugin_scope", func(db *gorm.DB) *gorm.DB {
			var p permissionScopeRow
			return pluginPermissionScope(db, "plugin:demo:read").Take(&p)
		}},
		{"granted_scopes", func(db *gorm.DB) *gorm.DB { return scan(grantedScopes(db, "plugin:demo:read")) }},
	})
}

// A role's grants: every write replaces them within a part.
func TestSQLAuthzGrantWrites(t *testing.T) {
	for name, part := range map[string]grantPart{"all": {}, "core": {coreOnly: true}, "plugin": {plugin: "demo"}} {
		t.Run(name, func(t *testing.T) {
			assertCases(t, []sqlCase{
				{"drop", func(db *gorm.DB) *gorm.DB { return dropGrants(db, 3, part, store.Int64s{11, 12}) }},
				{"drop_scoped", func(db *gorm.DB) *gorm.DB {
					return dropScopedGrants(db, 3, part, store.Int64s{13}, store.Strings{"own"})
				}},
			})
		})
	}
	assertCases(t, []sqlCase{
		{"add", func(db *gorm.DB) *gorm.DB { return addGrants(db, 3, store.Int64s{11, 12}) }},
		{"add_scoped", func(db *gorm.DB) *gorm.DB { return addScopedGrants(db, 3, store.Int64s{13}, store.Strings{"own"}) }},
	})
}

// The role writes. updated_at moves where the raw SQL moved it (update,
// permissions, assignable list) and nowhere else (reset text, defaults_rev).
func TestSQLAuthzRoleWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"insert", func(db *gorm.DB) *gorm.DB {
			var id int64
			return insertRole(db, CreateRoleInput{Key: "custom", Name: core.LocalizedText{"en": "Custom"}, Description: core.LocalizedText{}}).Scan(&id)
		}},
		{"update", func(db *gorm.DB) *gorm.DB {
			return updateRole(db, 3, UpdateRoleInput{Name: core.LocalizedText{"en": "Renamed"}})
		}},
		{"delete", func(db *gorm.DB) *gorm.DB { return deleteRole(db, 3) }},
		{"reset_text", func(db *gorm.DB) *gorm.DB {
			return resetRoleText(db, 3, builtinRole{name: core.LocalizedText{"en": "Admin"}, description: core.LocalizedText{}})
		}},
		{"defaults_rev", func(db *gorm.DB) *gorm.DB { return setDefaultsRev(db, 3, 4) }},
		{"touch", func(db *gorm.DB) *gorm.DB { return touchRole(db, 3) }},
		{"drop_assignable", func(db *gorm.DB) *gorm.DB { return dropAssignable(db, 3, store.Int64s{4, 5}) }},
		{"add_assignable", func(db *gorm.DB) *gorm.DB { return addAssignable(db, 3, store.Int64s{4, 5}) }},
		{"assign_custom", func(db *gorm.DB) *gorm.DB { return setAssignCustom(db, 3, false) }},
	})
}

// Role assignment and the super admin transfer.
func TestSQLAuthzUserRoleWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"drop", func(db *gorm.DB) *gorm.DB { return dropUserRoles(db, 7, store.Int64s{}) }},
		{"add", func(db *gorm.DB) *gorm.DB { return addUserRoles(db, 7, store.Int64s{2, 3}) }},
		{"revoke_superuser", func(db *gorm.DB) *gorm.DB { return revokeSuperuser(db, []int64{7}) }},
		{"grant_admin", func(db *gorm.DB) *gorm.DB { return grantAdmin(db, []int64{7}) }},
		{"grant_superuser", func(db *gorm.DB) *gorm.DB { return grantSuperuser(db, 9) }},
	})
}

// The plugin catalog writes.
func TestSQLAuthzPluginWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"upsert", func(db *gorm.DB) *gorm.DB {
			var row struct{ Inserted, WasRemoved bool }
			return upsertPermission(db, pluginPermissionRow{
				key: "plugin:demo:read", module: "plugin:demo", pluginKey: "demo", risk: manifest.RiskLow, scopeType: "plugin:item",
				group: "g", label: core.LocalizedText{"en": "Read"}, description: core.LocalizedText{}, groupLabel: core.LocalizedText{},
				sort: 1, groupSort: 2, scope: manifest.PermissionScope{Own: true}, requires: []string{"plugin:demo:list"},
				defaults: map[string][]string{RoleAdmin: {""}}, staged: true,
			}).Scan(&row)
		}},
		{"remove_undeclared", func(db *gorm.DB) *gorm.DB { return scan(removeUndeclared(db, "demo", []string{"plugin:demo:read"})) }},
		{"revoke_scopes", func(db *gorm.DB) *gorm.DB { return scan(revokeScopes(db, "plugin:demo:read", store.Strings{"own"})) }},
		{"grant", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(grantPluginSQL, store.Strings{RoleAdmin, RoleOperator}, store.Strings{"", "own"}, "plugin:demo:read"))
		}},
		{"set_status", func(db *gorm.DB) *gorm.DB { return setPluginStatus(db, "demo", "active") }},
		{"lock_staged", func(db *gorm.DB) *gorm.DB {
			var keys []string
			return lockStaged(db, "demo").Pluck("key", &keys)
		}},
		{"delete_keys", func(db *gorm.DB) *gorm.DB { return deletePermissions(db, []string{"plugin:demo:read"}) }},
		{"delete_plugin", func(db *gorm.DB) *gorm.DB { return deletePluginPermissions(db, "demo") }},
	})
}

// SyncCore's writes.
func TestSQLAuthzSyncWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"roles", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(syncRolesSQL, store.Strings{RoleAdmin}, store.Strings{`{"en":"Admin"}`}, store.Strings{`{}`},
				store.Bools{false}, store.Bools{true}, store.Int64s{2}))
		}},
		{"permissions", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(syncPermissionsSQL, store.Strings{"account:read"}, store.Strings{"account"}, store.Strings{`{"en":"Read"}`},
				store.Strings{`{}`}, store.Strings{"low"}, store.Strings{`[]`}, store.Int64s{1}, store.Strings{"account"}))
		}},
		{"advance", func(db *gorm.DB) *gorm.DB {
			return scan(advanceRoles(db, store.Strings{RoleAdmin}, store.Int64s{2}, store.Bools{true}))
		}},
		{"default_permissions", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(grantDefaultPermissionsSQL, RoleAdmin, store.Strings{"account:read"}))
		}},
		{"default_assignable", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(grantDefaultAssignableSQL, RoleAdmin, store.Strings{RoleUser}))
		}},
		{"scoped_defaults", func(db *gorm.DB) *gorm.DB {
			return scan(db.Raw(grantScopedDefaultsSQL, store.Strings{"account:read:own"}, RoleOperator))
		}},
		{"remove_undeclared", func(db *gorm.DB) *gorm.DB { return removeUndeclaredCore(db, store.Strings{"account:read"}) }},
	})
}

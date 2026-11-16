package authz

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
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

// The version: Bump locks and increments it (every authz write), SyncCore
// locks it without a bump; the readers select it with store.AuthzVersion.
func TestSQLAuthzVersion(t *testing.T) {
	assertCases(t, []sqlCase{
		{"bump", func(db *gorm.DB) *gorm.DB { return scan(bumpVersion(db)) }},
		{"lock", lockVersion},
	})
}

// The reads of an authorization check (loadPermissionSet, ensureCatalog)
// and of the grant view (RolesPermissions, usersRoles, grantOf).
func TestSQLAuthzPermissionLoad(t *testing.T) {
	assertCases(t, []sqlCase{
		{"catalog", func(db *gorm.DB) *gorm.DB { return scan(catalogRows(db)) }},
		{"role_grants", func(db *gorm.DB) *gorm.DB { return scan(roleGrants(db, []string{RoleAdmin, "custom"})) }},
		{"role_assignables", func(db *gorm.DB) *gorm.DB { return scan(roleAssignables(db, []string{RoleAdmin})) }},
		{"custom_roles", func(db *gorm.DB) *gorm.DB { return scan(customRoleKeys(db)) }},
		{"custom_roles_but", func(db *gorm.DB) *gorm.DB { return scan(customRoleKeysBut(db, "custom")) }},
	})
}

// The role reads, the locked one of editableRole included, and the checks
// that guard the super admin.
func TestSQLAuthzRoleReads(t *testing.T) {
	assertCases(t, []sqlCase{
		{"list", func(db *gorm.DB) *gorm.DB { return scan(listedRoles(db)) }},
		{"lock", func(db *gorm.DB) *gorm.DB { return scan(lockRole(db, 3)) }},
		{"by_id", func(db *gorm.DB) *gorm.DB { return scan(roleByID(db, 3)) }},
		{"by_key", func(db *gorm.DB) *gorm.DB { return scan(roleByKey(db, RoleAdmin)) }},
		{"by_keys", func(db *gorm.DB) *gorm.DB { return scan(rolesByKeys(db, []string{RoleAdmin, RoleUser})) }},
		{"user_roles", func(db *gorm.DB) *gorm.DB { return scan(userRoleRefs(db, 7)) }},
		{"role_refs", func(db *gorm.DB) *gorm.DB { return scan(roleRefsByKeys(db, []string{RoleAdmin, "custom"})) }},
		{"custom_role_refs", func(db *gorm.DB) *gorm.DB { return scan(customRoleRefsByKeys(db, []string{RoleAdmin})) }},
		{"assignable_builtin_refs", func(db *gorm.DB) *gorm.DB {
			return scan(assignableBuiltinRefs(db, []string{RoleOperator, RoleUser}))
		}},
		{"assignable", func(db *gorm.DB) *gorm.DB { return scan(assignableRoleRows(db)) }},
		{"is_superuser", func(db *gorm.DB) *gorm.DB { return scan(store.ExistsQuery(db, superuserGrants(db, 7))) }},
		{"other_superusers", func(db *gorm.DB) *gorm.DB { return scan(otherSuperusers(db, 7)) }},
		{"lock_transfer_target", func(db *gorm.DB) *gorm.DB { return scan(lockTransferTarget(db, 9)) }},
	})
}

func TestSQLAuthzPermissionReads(t *testing.T) {
	assertCases(t, []sqlCase{
		{"grantable", func(db *gorm.DB) *gorm.DB { return scan(grantablePermissions(db, []string{"account:read"})) }},
		{"list", func(db *gorm.DB) *gorm.DB { return scan(permissionRows(db, "demo")) }},
		{"holders", func(db *gorm.DB) *gorm.DB { return scan(holderRows(db, []string{"plugin:demo:read"})) }},
		{"plugin", func(db *gorm.DB) *gorm.DB { return scan(pluginPermissions(db, "demo")) }},
		// SetPluginRoleGrants: whether the plugin has any permission.
		{"plugin_exists", func(db *gorm.DB) *gorm.DB { return scan(store.ExistsQuery(db, permissionsOfPlugin(db, "demo"))) }},
		// GrantPluginPermission reads it with Row: one row, no LIMIT.
		{"plugin_scope", func(db *gorm.DB) *gorm.DB { return scan(pluginPermissionScope(db, "plugin:demo:read")) }},
		{"granted_scopes", func(db *gorm.DB) *gorm.DB { return scan(grantedScopes(db, "plugin:demo:read")) }},
	})
}

// ClearScopeGrants: whether a scoped grant names the deleted rows, then
// the delete, its audit and the version; the patterns and ids are one
// array parameter each.
func TestSQLAuthzClearScopeGrants(t *testing.T) {
	patterns, actor := []string{`(^|/)account/3$`, `(^|/)account/4$`}, int64(9)
	assertCases(t, []sqlCase{
		{"exists", func(db *gorm.DB) *gorm.DB { return scan(store.ExistsQuery(db, casbinrule.ScopedGrants(db, patterns))) }},
		{"clear", func(db *gorm.DB) *gorm.DB {
			return scan(clearScopeGrants(db, patterns, &actor, core.ScopeRefAccount, []int64{3, 4}))
		}},
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
		{"set_status", func(db *gorm.DB) *gorm.DB { return setPluginStatus(db, "demo", "active") }},
		{"lock_staged", func(db *gorm.DB) *gorm.DB {
			var keys []string
			return lockStaged(db, "demo").Pluck("key", &keys)
		}},
		{"delete_keys", func(db *gorm.DB) *gorm.DB {
			return db.Where("key = ANY(?)", store.Strings{"plugin:demo:read"}).Delete(&model.Permission{})
		}},
	})
}

// SyncCore's writes.
func TestSQLAuthzSyncWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"roles", func(db *gorm.DB) *gorm.DB {
			return scan(syncRoles(db, store.Strings{RoleAdmin}, store.Strings{`{"en":"Admin"}`}, store.Strings{`{}`},
				store.Bools{false}, store.Bools{true}, store.Int64s{2}))
		}},
		{"permissions", func(db *gorm.DB) *gorm.DB {
			return scan(syncPermissions(db, store.Strings{"account:read"}, store.Strings{"account"}, store.Strings{`{"en":"Read"}`},
				store.Strings{`{}`}, store.Strings{"low"}, store.Strings{`[]`}, store.Int64s{1}, store.Strings{"account"}))
		}},
		{"advance", func(db *gorm.DB) *gorm.DB {
			return scan(advanceRoles(db, store.Strings{RoleAdmin}, store.Int64s{2}, store.Bools{true}))
		}},
		{"remove_undeclared", func(db *gorm.DB) *gorm.DB { return removeUndeclaredCore(db, store.Strings{"account:read"}) }},
	})
	t.Run("default_assignable", func(t *testing.T) {
		gormtest.AssertRun(t, func(db *gorm.DB) error {
			_, err := grantDefaultAssignable(db, RoleAdmin, []string{RoleUser})
			return err
		})
	})
}

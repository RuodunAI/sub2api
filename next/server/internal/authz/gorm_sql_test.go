package authz

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL of the authz statements built with GORM (see package gormtest):
// every write, the version and the reads behind each authorization check.
// A function that runs its statements is snapshot with AssertRun: DryRun
// returns no rows, so what it writes from what it read is not reached.

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

// runCase is one AssertRun snapshot.
type runCase struct {
	name string
	run  func(*gorm.DB) error
}

func assertRuns(t *testing.T, cases []runCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { gormtest.AssertRun(t, c.run) })
	}
}

// scan finishes a chained query for its snapshot.
func scan(q *gorm.DB) *gorm.DB {
	var dest []map[string]any
	return q.Scan(&dest)
}

// refused is err but a refusal (a *core.Error), which a check reading no
// rows under DryRun gives.
func refused(err error) error {
	if ce := (*core.Error)(nil); errors.As(err, &ce) {
		return nil
	}
	return err
}

// The versions: bumpVersions increments the authz version (a user write,
// Bump) or both (a policy write, bumpPolicy), lockVersion locks them
// without a bump (SyncCore, ClearScopeGrants); the readers select them with
// store.AuthzVersions.
func TestSQLAuthzVersion(t *testing.T) {
	assertRuns(t, []runCase{
		{"bump", func(db *gorm.DB) error { _, err := bumpVersions(db, false); return noRows(err) }},
		{"bump_policy", func(db *gorm.DB) error { _, err := bumpVersions(db, true); return noRows(err) }},
		{"lock", lockVersion},
	})
}

// noRows is err but sql.ErrNoRows, what an UPDATE under DryRun reports.
func noRows(err error) error {
	if store.IsNoRows(err) {
		return nil
	}
	return err
}

// The reads of the grant view (RolesPermissions, usersRoles, grantOf).
func TestSQLAuthzPermissionLoad(t *testing.T) {
	assertCases(t, []sqlCase{
		{"role_grants", func(db *gorm.DB) *gorm.DB { return scan(roleGrants(db, []string{RoleAdmin, "custom"})) }},
		{"role_assignables", func(db *gorm.DB) *gorm.DB { return scan(roleAssignables(db, []string{RoleAdmin})) }},
	})
	assertRuns(t, []runCase{
		{"custom_roles", func(db *gorm.DB) error { _, err := customRoleKeys(db, "custom"); return err }},
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
		{"assignable_builtin_refs", func(db *gorm.DB) *gorm.DB {
			return scan(assignableBuiltinRefs(db, []string{RoleOperator, RoleUser}))
		}},
		{"assignable", func(db *gorm.DB) *gorm.DB { return scan(assignableRoleRows(db)) }},
		{"is_superuser", func(db *gorm.DB) *gorm.DB { return scan(store.ExistsQuery(db, superuserGrants(db, 7))) }},
		{"other_superusers", func(db *gorm.DB) *gorm.DB { var n int64; return otherSuperusers(db, 7).Count(&n) }},
		{"lock_transfer_target", func(db *gorm.DB) *gorm.DB { return scan(lockTransferTarget(db, 9)) }},
	})
	assertRuns(t, []runCase{
		{"custom_role_refs", builtinKeyConflict},
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
	})
}

// ClearScopeGrants: the lock and the delete (the audit, the roles'
// revisions and the versions follow only when a row went); and the check
// of a grant write that the rows its scopes name exist, a read per kind.
func TestSQLAuthzClearScopeGrants(t *testing.T) {
	assertRuns(t, []runCase{
		{"missing_refs", func(db *gorm.DB) error {
			return refused(checkScopeRefs(db.Statement.Context, db, []string{"account:read:group/2/account/3", "account:read:group/2/account/3", "proxy:read:proxy/4"}))
		}},
		{"clear", func(db *gorm.DB) error {
			_, err := new(Service).ClearScopeGrants(db.Statement.Context, db, 9, core.ScopeRefAccount, []int64{3, 4})
			return err
		}},
	})
}

// The role writes. updated_at moves where the raw SQL moved it (update,
// permissions, assignable list) and nowhere else (reset text, defaults_rev).
func TestSQLAuthzRoleWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"update", func(db *gorm.DB) *gorm.DB {
			return updateRole(db, 3, UpdateRoleInput{Name: core.LocalizedText{"en": "Renamed"}})
		}},
		{"delete", func(db *gorm.DB) *gorm.DB { return deleteRole(db, 3) }},
		{"reset_text", func(db *gorm.DB) *gorm.DB {
			return resetRoleText(db, 3, builtinRole{name: core.LocalizedText{"en": "Admin"}, description: core.LocalizedText{}})
		}},
		{"defaults_rev", func(db *gorm.DB) *gorm.DB { return setDefaultsRev(db, 3, 4) }},
		{"touch", func(db *gorm.DB) *gorm.DB { return touchRole(db, 3) }},
		{"revise", func(db *gorm.DB) *gorm.DB { return reviseRole(db, 3) }},
		{"revise_listing", func(db *gorm.DB) *gorm.DB { return reviseListing(db, 3) }},
		{"drop_assignable", func(db *gorm.DB) *gorm.DB { return dropAssignable(db, 3, store.Int64s{4, 5}) }},
		{"assign_custom", func(db *gorm.DB) *gorm.DB { return setAssignCustom(db, 3, false) }},
	})
	assertRuns(t, []runCase{
		{"insert", func(db *gorm.DB) error {
			_, err := insertRole(db, CreateRoleInput{Key: "custom", Name: core.LocalizedText{"en": "Custom"}, Description: core.LocalizedText{}})
			return err
		}},
		{"add_assignable", func(db *gorm.DB) error { return addAssignable(db, 3, []int64{4, 5}) }},
	})
}

// The plugin catalog writes. The upsert reads the key's row first; under
// DryRun there is none, so it inserts (staged: no plugin status read).
func TestSQLAuthzPluginWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"set_status", func(db *gorm.DB) *gorm.DB { return setPluginStatus(db, "demo", "active") }},
		{"lock_staged", func(db *gorm.DB) *gorm.DB {
			var keys []string
			return lockStaged(db, "demo").Pluck("key", &keys)
		}},
		{"delete_keys", func(db *gorm.DB) *gorm.DB {
			return db.Where("key = ANY(?)", store.Strings{"plugin:demo:read"}).Delete(&model.Permission{})
		}},
	})
	assertRuns(t, []runCase{
		{"upsert", func(db *gorm.DB) error {
			_, _, err := upsertPluginPermission(db.Statement.Context, db, "demo", "read", core.PermissionDef{
				Key: "plugin:demo:read", Risk: manifest.RiskLow, ScopeType: "plugin:item", Group: "g", Label: core.LocalizedText{"en": "Read"},
				Sort: 1, GroupSort: 2, Scope: &manifest.PermissionScope{Own: true}, Requires: []string{"plugin:demo:list"},
				Defaults: map[string][]string{RoleAdmin: {""}}, Staged: true,
			})
			return err
		}},
		{"remove_undeclared", func(db *gorm.DB) error {
			_, err := removeUndeclared(db, "demo", []string{"plugin:demo:read"})
			return err
		}},
	})
}

// SyncCore's reads and writes; under DryRun every built-in role and the
// one permission are missing, so they are inserted.
func TestSQLAuthzSyncWrites(t *testing.T) {
	assertCases(t, []sqlCase{
		{"remove_undeclared", func(db *gorm.DB) *gorm.DB { return removeUndeclaredCore(db, store.Strings{"account:read"}) }},
	})
	assertRuns(t, []runCase{
		{"roles", func(db *gorm.DB) error { return syncRoles(db, func(string, bool) {}) }},
		{"permissions", func(db *gorm.DB) error {
			_, _, err := syncPermissions(db, []core.PermissionDef{{Key: "account:read", Module: "account", Label: core.LocalizedText{"en": "Read"},
				Description: core.LocalizedText{}, Risk: manifest.RiskLow, Sort: 1, ScopeType: "account"}})
			return err
		}},
		{"advance", func(db *gorm.DB) error {
			return new(Service).advanceDefaults(db.Statement.Context, db, func(string, int32, bool) {})
		}},
		{"default_assignable", func(db *gorm.DB) error {
			_, err := grantDefaultAssignable(db, RoleAdmin, []string{RoleUser})
			return err
		}},
	})
}

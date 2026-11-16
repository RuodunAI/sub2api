package casbinrule

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of every statement on casbin_rule (see package gormtest): the
// review surface of the policy's mapping.

func assertRuns(t *testing.T, cases map[string]func(*gorm.DB) error) {
	t.Helper()
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { gormtest.AssertRun(t, run) })
	}
}

func TestSQLCasbinRuleReads(t *testing.T) {
	var dest []map[string]any
	assertRuns(t, map[string]func(*gorm.DB) error{
		"role_keys_of": func(db *gorm.DB) error { return RoleKeysOf(db, []int64{7, 8}).Scan(&dest).Error },
		"user_role_subs": func(db *gorm.DB) error {
			_, err := UserRoleSubs(db, 7)
			return err
		},
		"lock_members": func(db *gorm.DB) error {
			_, err := LockMembers(db, "super_admin")
			return err
		},
		"scoped_grants": func(db *gorm.DB) error {
			return ScopedGrants(db, []string{`^group:5(:|$)`}).Find(&dest).Error
		},
		"grants_of": func(db *gorm.DB) error { return db.Raw(GrantsOf("?"), "admin").Scan(&dest).Error },
		"roles_of":  func(db *gorm.DB) error { return db.Raw(RolesOf("?"), 7).Scan(&dest).Error },
		"members_of": func(db *gorm.DB) error {
			return db.Raw(MembersOf("?"), "admin").Scan(&dest).Error
		},
		"delete_scoped": func(db *gorm.DB) error {
			return db.Raw(DeleteScopedSQL, store.Strings{`^group:5(:|$)`}).Scan(&dest).Error
		},
		"orphans": func(db *gorm.DB) error { return db.Raw(OrphansSQL).Scan(&dest).Error },
	})
}

func TestSQLCasbinRuleWrites(t *testing.T) {
	grants := []Grant{{Role: "admin", Action: "account:read"}, {Role: "operator", Action: "account:read", Suffix: "own"}}
	assertRuns(t, map[string]func(*gorm.DB) error{
		"add_grants": func(db *gorm.DB) error {
			_, err := AddGrants(db, grants, gorm.Expr("p.source = ?", "core"))
			return err
		},
		"drop_grants": func(db *gorm.DB) error {
			return DropGrants(db, "admin", gorm.Expr("p.plugin_key = ?", "demo"), grants[:1])
		},
		"revoke_scopes": func(db *gorm.DB) error {
			_, err := RevokeScopes(db, "plugin:demo:read", []string{"own"})
			return err
		},
		"drop_permissions": func(db *gorm.DB) error { return DropPermissions(db, []string{"plugin:demo:read"}) },
		"sync_superusers": func(db *gorm.DB) error {
			_, err := SyncSuperusers(db)
			return err
		},
		"delete_role":    func(db *gorm.DB) error { return DeleteRole(db, "custom") },
		"set_roles":      func(db *gorm.DB) error { return SetRoles(db, 7, []string{"admin", "user"}) },
		"add_members":    func(db *gorm.DB) error { return AddMembers(db, "admin", []int64{7, 8}) },
		"remove_members": func(db *gorm.DB) error { return RemoveMembers(db, "super_admin", []int64{7}) },
	})
}

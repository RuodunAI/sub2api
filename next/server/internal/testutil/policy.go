package testutil

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Policy fixtures: casbin_rule rows written as is, behind the authz
// service's back (no grant checks, no audit, no version move). A test that
// needs a cache to see them moves the version itself (BumpAuthz).

// Grant gives the role key the permission perm: for every row without
// scopes, else one grant per scope (a canonical suffix such as "own").
func Grant(tb testing.TB, db *store.DB, role, perm string, scopes ...string) {
	tb.Helper()
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	rows := make([]model.CasbinRule, len(scopes))
	for i, sc := range scopes {
		rows[i] = casbinrule.PolicyRule(casbinrule.Grant{Role: role, Action: perm, Suffix: sc})
	}
	if err := casbinrule.Insert(db.Gorm, rows); err != nil {
		tb.Fatalf("grant %s %s %v: %v", role, perm, scopes, err)
	}
}

// Revoke takes from the role key the permission perm, every grant of it
// (each scope and every row).
func Revoke(tb testing.TB, db *store.DB, role, perm string) {
	tb.Helper()
	if err := casbinrule.DropGrants(db.Gorm, role, gorm.Expr("p.key = ?", perm), nil); err != nil {
		tb.Fatal(err)
	}
}

// AssignRoles gives the user userID the roles keys.
func AssignRoles(tb testing.TB, db *store.DB, userID int64, keys ...string) {
	tb.Helper()
	rows := make([]model.CasbinRule, len(keys))
	for i, k := range keys {
		rows[i] = casbinrule.RoleRule(userID, k)
	}
	if err := casbinrule.Insert(db.Gorm, rows); err != nil {
		tb.Fatalf("assign %v to %d: %v", keys, userID, err)
	}
}

// UnassignRoles takes every role from the user userID.
func UnassignRoles(tb testing.TB, db *store.DB, userID int64) {
	tb.Helper()
	if err := casbinrule.SetRoles(db.Gorm, userID, nil); err != nil {
		tb.Fatal(err)
	}
}

// RoleKeys returns the keys of the roles the user userID holds, sorted.
func RoleKeys(tb testing.TB, db *store.DB, userID int64) []string {
	tb.Helper()
	keys := []string{}
	if err := casbinrule.RoleKeysOf(db.Gorm, []int64{userID}).Scan(&keys).Error; err != nil {
		tb.Fatal(err)
	}
	return keys
}

// GrantKeys returns the grants of the role key as canonical keys
// ("account:read", "account:read:own"), sorted.
func GrantKeys(tb testing.TB, db *store.DB, role string) []string {
	tb.Helper()
	keys := []string{}
	if err := db.Gorm.Raw(`SELECT key FROM (`+casbinrule.GrantsOf("?")+`) g ORDER BY key`, role).Scan(&keys).Error; err != nil {
		tb.Fatal(err)
	}
	return keys
}

// BumpAuthz moves the authz version, as a write through the service would:
// the caches read again what a fixture changed.
func BumpAuthz(tb testing.TB, db *store.DB) {
	tb.Helper()
	if err := db.Gorm.Exec(`UPDATE authz_meta SET version = version + 1 WHERE id = 1`).Error; err != nil {
		tb.Fatal(err)
	}
}

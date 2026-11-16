package testutil

import (
	"testing"

	"gorm.io/gorm/clause"

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
		if sc == "" {
			sc = casbinrule.AllRows
		}
		rows[i] = model.CasbinRule{Ptype: "p", V0: casbinrule.RoleSub(role), V1: perm, V2: sc}
	}
	if err := db.Gorm.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		tb.Fatalf("grant %s %s %v: %v", role, perm, scopes, err)
	}
}

// Revoke takes from the role key the permission perm, every grant of it
// (each scope and every row).
func Revoke(tb testing.TB, db *store.DB, role, perm string) {
	tb.Helper()
	if err := casbinrule.DropGrants(db.Gorm, role, casbinrule.Filter{Action: perm}, nil); err != nil {
		tb.Fatal(err)
	}
}

// AssignRoles gives the user userID the roles keys.
func AssignRoles(tb testing.TB, db *store.DB, userID int64, keys ...string) {
	tb.Helper()
	for _, k := range keys {
		if err := casbinrule.AddMembers(db.Gorm, k, []int64{userID}); err != nil {
			tb.Fatalf("assign %s to %d: %v", k, userID, err)
		}
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

// orphansSQL selects, as "<ptype> <v0> <v1> <v2>" sorted, the casbin_rule
// rows naming a user, role or permission that does not exist (the mapping
// of CONTRACTS §99.1): what a delete path that does not clear its rows
// leaves behind.
var orphansSQL = `SELECT concat_ws(' ', cr.ptype, cr.v0, cr.v1, NULLIF(cr.v2, '')) FROM ` + casbinrule.Table + ` cr
WHERE (cr.ptype = 'p' AND (
        NOT EXISTS (SELECT 1 FROM roles r WHERE 'r:' || r.key = cr.v0)
        OR (cr.v1 <> '` + casbinrule.SuperKey + `' AND NOT EXISTS (SELECT 1 FROM permissions p WHERE p.key = cr.v1))))
   OR (cr.ptype = 'g' AND (
        NOT EXISTS (SELECT 1 FROM users u WHERE 'u:' || u.id = cr.v0)
        OR NOT EXISTS (SELECT 1 FROM roles r WHERE 'r:' || r.key = cr.v1)))
ORDER BY 1`

// PolicyOrphans returns the casbin_rule rows naming a user, role or
// permission that does not exist (orphansSQL), sorted; DB checks it after
// every test.
func PolicyOrphans(tb testing.TB, db *store.DB) []string {
	tb.Helper()
	out := []string{}
	if err := db.Gorm.Raw(orphansSQL).Scan(&out).Error; err != nil {
		tb.Fatal(err)
	}
	return out
}

// BumpAuthz moves the authz version, as a write through the service would:
// the caches read again what a fixture changed.
func BumpAuthz(tb testing.TB, db *store.DB) {
	tb.Helper()
	if err := db.Gorm.Exec(`UPDATE authz_meta SET version = version + 1 WHERE id = 1`).Error; err != nil {
		tb.Fatal(err)
	}
}

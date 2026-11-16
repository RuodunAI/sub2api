package authz

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func transfer(s *Service, actor, target int64) ([]int64, error) {
	return transferWith(s, s.TransferSuperAdmin, actor, target)
}

// transferWith runs move (TransferSuperAdmin or its Legacy form) in a
// transaction, as iam does, and announces the version.
func transferWith(s *Service, move func(context.Context, *gorm.DB, int64, int64) (int64, []int64, error), actor, target int64) ([]int64, error) {
	ctx := context.Background()
	var v int64
	var demoted []int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		var err error
		v, demoted, err = move(ctx, tx, actor, target)
		return err
	})
	if err == nil {
		s.Committed(ctx, v)
	}
	return demoted, err
}

// superAdminsNow lists the holders of the super admin role, as the
// transfer reads them.
func superAdminsNow(t *testing.T, s *Service) []int64 { return superAdminsIn(t, s, policyHolders{}) }

// superAdminsIn lists the holders of the super admin role in h.
func superAdminsIn(t *testing.T, s *Service, h holderStore) []int64 {
	t.Helper()
	var ids []int64
	err := s.db.Tx(context.Background(), func(tx *gorm.DB) error {
		var err error
		ids, err = h.lock(tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func userRoleKeys(t *testing.T, s *Service, uid int64) []string {
	t.Helper()
	return testutil.RoleKeys(t, s.db, uid)
}

func TestTransferSuperAdmin(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, s, "admin@x.com", RoleAdmin)
	target := addUser(t, s, "target@x.com", RoleUser)
	disabled := addUser(t, s, "disabled@x.com", RoleUser)
	deleted := addUser(t, s, "deleted@x.com", RoleUser)
	if _, err := db.SQL.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}

	denied := []struct {
		name          string
		actor, target int64
		code          string
	}{
		{"not the super admin", admin, target, "permission_denied"},
		{"to self", root, root, "invalid_argument"},
		{"disabled target", root, disabled, "invalid_argument"},
		{"deleted target", root, deleted, "invalid_argument"},
		{"unknown target", root, 1 << 40, "invalid_argument"},
	}
	for _, tt := range denied {
		if _, err := transfer(s, tt.actor, tt.target); !isCode(err, tt.code) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	if got := superAdminsNow(t, s); !slices.Equal(got, []int64{root}) {
		t.Fatalf("holders after refusals: %v", got)
	}

	demoted, err := transfer(s, root, target)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(demoted, []int64{root}) || !slices.Equal(superAdminsNow(t, s), []int64{target}) {
		t.Fatalf("demoted %v, holders %v", demoted, superAdminsNow(t, s))
	}
	if got := userRoleKeys(t, s, root); !slices.Equal(got, []string{RoleAdmin}) {
		t.Fatalf("old holder roles: %v", got)
	}
	if got := userRoleKeys(t, s, target); !slices.Equal(got, []string{RoleSuperAdmin, RoleUser}) {
		t.Fatalf("new holder roles: %v", got)
	}
	mustCan(t, s, target, "plugin:manage", true)
	var n int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'superadmin.transfer' AND user_id = $1 AND target_id = $2
		AND detail->>'via' = 'console' AND detail->'from' = $3::jsonb`, root, strconv.FormatInt(target, 10), "["+strconv.FormatInt(root, 10)+"]").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
	if _, err := transfer(s, root, admin); !isCode(err, "permission_denied") {
		t.Fatalf("former holder transferred again: %v", err)
	}
}

// The CLI (actor 0) also repairs a database with no holder, or one that
// is deleted: a deleted holder loses the role without becoming an admin, so
// restoring the user later brings back no privileges.
func TestTransferSuperAdminRepairs(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	a := addUser(t, s, "a@x.com", RoleSuperAdmin, RoleUser)
	b := addUser(t, s, "b@x.com", RoleUser)
	c := addUser(t, s, "c@x.com", RoleUser)
	if _, err := db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	demoted, err := transfer(s, 0, b)
	if err != nil || !slices.Equal(demoted, []int64{a}) || !slices.Equal(superAdminsNow(t, s), []int64{b}) {
		t.Fatalf("deleted holder: demoted %v, holders %v, %v", demoted, superAdminsNow(t, s), err)
	}
	if got := userRoleKeys(t, s, a); !slices.Equal(got, []string{RoleUser}) {
		t.Fatalf("deleted holder roles: %v", got)
	}
	if err := casbinrule.RemoveMembers(db.Gorm, RoleSuperAdmin, []int64{b}); err != nil {
		t.Fatal(err)
	}
	demoted, err = transfer(s, 0, c)
	if err != nil || len(demoted) != 0 || !slices.Equal(superAdminsNow(t, s), []int64{c}) {
		t.Fatalf("no holder: demoted %v, holders %v, %v", demoted, superAdminsNow(t, s), err)
	}
	var via string
	if err := db.SQL.QueryRowContext(ctx, `SELECT detail->>'via' FROM audit_logs WHERE action = 'superadmin.transfer' AND target_id = $1`, strconv.FormatInt(c, 10)).Scan(&via); err != nil || via != "cli" {
		t.Fatalf("via %q %v", via, err)
	}
}

// The console's transfer (and the CLI's on a migrated database) writes
// casbin_rule whatever else the database holds: a table named user_roles
// left after 0100 (a restore of one table, a stray CREATE) is no store.
func TestTransferSuperAdminIgnoresStrayUserRoles(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	target := addUser(t, s, "target@x.com", RoleUser)
	if _, err := db.SQL.ExecContext(context.Background(), `CREATE TABLE user_roles (user_id bigint, role_id bigint)`); err != nil {
		t.Fatal(err)
	}
	if _, err := transfer(s, root, target); err != nil || !slices.Equal(superAdminsNow(t, s), []int64{target}) {
		t.Fatalf("holders %v, %v", superAdminsNow(t, s), err)
	}
}

// On the schema before migration 0100 (user_roles, whose 0097 check of one
// holder points the operator at the CLI), the CLI repairs several holders:
// every other one becomes an admin, a deleted one only loses the role.
func TestTransferSuperAdminRepairsLegacy(t *testing.T) {
	t.Parallel()
	db := testutil.DBThrough(t, "0099_plugin_permission_meta.sql")
	s := New(Deps{DB: db})
	ctx := context.Background()
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO roles (key, name, superuser) VALUES ($1, '{}', true), ($2, '{}', false), ($3, '{}', false)`,
		RoleSuperAdmin, RoleAdmin, RoleUser); err != nil {
		t.Fatal(err)
	}
	users := map[string]int64{}
	for _, name := range []string{"a", "b", "c", "d"} {
		users[name] = testutil.InsertUser(t, db, name+"@x.com")
	}
	a, b, d := users["a"], users["b"], users["d"]
	testutil.ExecWithoutTrigger(t, db, "user_roles", "user_roles_one_superuser",
		`INSERT INTO user_roles (user_id, role_id) SELECT u, r.id FROM unnest($1::bigint[]) u, roles r WHERE r.key = $2`, []int64{a, b, d}, RoleSuperAdmin)
	if _, err := db.SQL.ExecContext(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, d); err != nil {
		t.Fatal(err)
	}
	demoted, err := transferWith(s, s.TransferSuperAdminLegacy, 0, b)
	if holders := superAdminsIn(t, s, legacyHolders{}); err != nil || !slices.Equal(demoted, []int64{a, d}) || !slices.Equal(holders, []int64{b}) {
		t.Fatalf("several holders: demoted %v, holders %v, %v", demoted, holders, err)
	}
	roles := func(uid int64) []string {
		var keys []string
		if err := db.SQL.QueryRowContext(ctx, `SELECT COALESCE(array_agg(r.key ORDER BY r.key), '{}') FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1`, uid).Scan(&keys); err != nil {
			t.Fatal(err)
		}
		return keys
	}
	if got := roles(a); !slices.Equal(got, []string{RoleAdmin}) {
		t.Fatalf("demoted holder roles: %v", got)
	}
	if got := roles(d); len(got) != 0 {
		t.Fatalf("deleted holder roles: %v", got)
	}
}

// Bump serializes transfers: of two at once by the same holder, the second
// finds it no longer holds the role.
func TestTransferSuperAdminConcurrent(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	root := addUser(t, s, "root@x.com", RoleSuperAdmin)
	targets := []int64{addUser(t, s, "a@x.com", RoleUser), addUser(t, s, "b@x.com", RoleUser)}
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = transfer(s, root, target)
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("both or neither succeeded: %v", errs)
	}
	for _, err := range errs {
		if err != nil && !isCode(err, "permission_denied") {
			t.Fatalf("loser: %v", err)
		}
	}
	if got := superAdminsNow(t, s); len(got) != 1 || !slices.Contains(targets, got[0]) {
		t.Fatalf("holders: %v", got)
	}
}

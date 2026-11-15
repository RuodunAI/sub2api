package authz

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// requireProjection skips a test on a schema without the casbin_rule
// projection (migration 0100).
func requireProjection(t *testing.T, db *store.DB) {
	t.Helper()
	var present bool
	if err := db.Gorm.Raw(`SELECT to_regclass('authz_casbin_drift') IS NOT NULL`).Row().Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Skip("no casbin_rule projection (migration 0100) in this schema")
	}
}

func driftRows(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var n int64
	if err := casbinDrift(db.Gorm).Row().Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCasbinDriftTaskRepairs(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	requireProjection(t, db)
	s := newService(t, db, nil, nil)
	addUser(t, s, "u@x.com", RoleUser)
	run := s.casbinDriftSpec().Handler

	// No drift: nothing repaired, the version stays.
	v := s.Version()
	if res, err := run(ctx, core.TaskRun{Name: CasbinDriftTask}); err != nil || res.Count != 0 {
		t.Fatalf("clean run: %+v, %v", res, err)
	}
	if s.Version() != v {
		t.Fatalf("clean run moved the version %d -> %d", v, s.Version())
	}

	// Both directions: a projected row lost, a row nothing projects.
	if err := db.Gorm.Exec(`DELETE FROM casbin_rule WHERE id = (SELECT min(id) FROM casbin_rule WHERE ptype = 'g')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Gorm.Exec(`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('p', 'r:nobody', 'user:read', '*')`).Error; err != nil {
		t.Fatal(err)
	}
	if n := driftRows(t, db); n != 2 {
		t.Fatalf("drift %d, want 2", n)
	}
	res, err := run(ctx, core.TaskRun{Name: CasbinDriftTask})
	if err != nil || res.Count != 2 || res.Message == "" {
		t.Fatalf("repair run: %+v, %v", res, err)
	}
	if n := driftRows(t, db); n != 0 {
		t.Fatalf("drift after the repair: %d", n)
	}
	if s.Version() <= v {
		t.Fatalf("the repair did not move the version (%d)", s.Version())
	}
}

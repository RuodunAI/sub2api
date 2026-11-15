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

// The drift is counted again under the authz_meta lock: a repair made by
// someone else while the task waited for the lock leaves nothing to do, and
// the task then rolls back without moving the version.
func TestCasbinDriftRecountedUnderLock(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	requireProjection(t, db)
	s := newService(t, db, nil, nil)
	addUser(t, s, "u@x.com", RoleUser)
	if err := db.Gorm.Exec(`DELETE FROM casbin_rule WHERE id = (SELECT min(id) FROM casbin_rule WHERE ptype = 'g')`).Error; err != nil {
		t.Fatal(err)
	}

	// Another writer holds the lock and repairs the drift.
	other, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback() }()
	if _, err := other.ExecContext(ctx, `SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `SELECT authz_casbin_repair()`); err != nil {
		t.Fatal(err)
	}
	v := version(t, s)
	type result struct{ drift, fixed int64 }
	done := make(chan result, 1)
	go func() {
		drift, fixed, err := s.RepairCasbinDrift(ctx)
		if err != nil {
			t.Error(err)
		}
		done <- result{drift, fixed}
	}()
	// The task counted the drift (not yet repaired for it) and waits for
	// the lock.
	eventually(t, func() bool {
		var waiting int
		_ = db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		return waiting > 0
	})
	if err := other.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r != (result{0, 0}) {
		t.Fatalf("repair under the lock: %+v, want nothing", r)
	}
	if got := version(t, s); got != v {
		t.Fatalf("version moved %d -> %d with nothing repaired", v, got)
	}
	if n := driftRows(t, db); n != 0 {
		t.Fatalf("drift %d", n)
	}
}

// version is the authz version in the database.
func version(t *testing.T, s *Service) int64 {
	t.Helper()
	v, err := s.currentVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

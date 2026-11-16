package cluster

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestCoreUpdateBusy(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	if busy, err := CoreUpdateBusy(ctx, db.Gorm); err != nil || busy {
		t.Fatal(busy, err)
	}
	if _, err := db.SQL.ExecContext(ctx, `CREATE SCHEMA updater; CREATE TABLE updater.upgrades(status text); INSERT INTO updater.upgrades VALUES('running')`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "paused", "completed", "cancelled", "failed", "superseded"} {
		if _, err := db.SQL.ExecContext(ctx, `UPDATE updater.upgrades SET status=$1`, state); err != nil {
			t.Fatal(err)
		}
		if busy, err := CoreUpdateBusy(ctx, db.Gorm); err != nil || busy != (state == "running" || state == "paused") {
			t.Fatal(state, busy, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := CoreUpdateBusy(cancelled, db.Gorm); err == nil {
		t.Fatal("database read failure hidden")
	}
}

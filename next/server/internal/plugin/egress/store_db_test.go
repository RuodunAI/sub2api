package egress

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// insertOpen returns each row's id at the row's position, and closeRows
// then closes exactly the rows those ids name (C1 of the GORM review: the
// ids came from the order of a multi-row INSERT's RETURNING, which
// PostgreSQL does not promise; a mismatch would close one connection's row
// with another's figures). The ids are read back by WITH ORDINALITY.
func TestInsertOpenIDsFollowTheRows(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	st := pgStore{db: db}
	key := fmt.Sprintf("ord%d", time.Now().UnixNano()%1_000_000_000)
	started := time.Now().UTC().Truncate(time.Millisecond)
	const n = 300
	rows := make([]logEntry, n)
	for i := range rows {
		rows[i] = logEntry{PluginKey: key, NodeID: "node-1", Network: "tcp", Host: fmt.Sprintf("h%03d.test", i),
			Port: 1000 + i, StartedAt: started.Add(time.Duration(n-i) * time.Second)}
	}
	ids, err := st.insertOpen(ctx, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != n {
		t.Fatalf("%d ids for %d rows", len(ids), n)
	}
	closes := make([]closedRow, 0, n/2)
	for i, id := range ids {
		var got model.PluginEgressLog
		if err := store.G(ctx, db.Gorm).Where("id = ?", id).Take(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got.Host != rows[i].Host || got.Port != rows[i].Port || got.Result != ResultOpen || got.ClosedAt != nil {
			t.Fatalf("id %d (row %d) is %+v, want host %s", id, i, got, rows[i].Host)
		}
		if i%2 == 0 {
			closes = append(closes, closedRow{ID: id, Result: ResultOK, DurationMS: i + 1, BytesIn: int64(i), BytesOut: int64(2 * i), ClosedAt: started})
		}
	}
	if err := st.closeRows(ctx, closes); err != nil {
		t.Fatal(err)
	}
	var logs []model.PluginEgressLog
	if err := store.G(ctx, db.Gorm).Where("plugin_key = ?", key).Order("port").Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	for i, l := range logs {
		closed := i%2 == 0
		if l.Host != rows[i].Host || (l.Result == ResultOK) != closed || closed && (l.DurationMs != i+1 || l.BytesIn != int64(i) || l.BytesOut != int64(2*i)) {
			t.Fatalf("row %d after the close: %+v", i, l)
		}
	}

	// Finished rows go in whole, closed at their start plus their duration.
	done := []logEntry{{PluginKey: key, NodeID: "node-1", Network: "tcp", Host: "done.test", Port: 1, StartedAt: started,
		DurationMS: 1500, BytesIn: 3, Result: "dial_error", Error: "refused"}}
	if err := st.insertDone(ctx, done); err != nil {
		t.Fatal(err)
	}
	var got model.PluginEgressLog
	if err := store.G(ctx, db.Gorm).Where("plugin_key = ? AND host = ?", key, "done.test").Take(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Error != "refused" || got.BytesIn != 3 || got.DurationMs != 1500 || got.ClosedAt == nil || !got.ClosedAt.Equal(started.Add(1500*time.Millisecond)) {
		t.Fatalf("finished row %+v", got)
	}
	if ids, err := st.insertOpen(ctx, nil); err != nil || len(ids) != 0 {
		t.Fatalf("insertOpen(nil) = %v, %v", ids, err)
	}
}

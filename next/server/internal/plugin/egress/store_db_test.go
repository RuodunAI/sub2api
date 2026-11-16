package egress

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// insertOpen returns each row's id at the row's position, and closeRows
// then closes exactly the rows those ids name (C1 of the GORM review: the
// ids came from the order of a multi-row INSERT's RETURNING, which
// PostgreSQL does not promise; a mismatch would close one connection's row
// with another's figures). The ids are reserved before the INSERT
// (gormpg.NextIDs).
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

// upsertDomains reports the hosts the batch added, each once with its own
// first sighting, and adds the counts of the known ones.
func TestUpsertDomainsReportsNewHosts(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	st := pgStore{db: db}
	key := fmt.Sprintf("dom%d", time.Now().UnixNano()%1_000_000_000)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	delta := func(host string, n int64, at time.Time) domainDelta {
		return domainDelta{PluginKey: key, Host: host, Connections: n, FirstSeen: at, LastSeen: at}
	}
	upsert := func(ds ...domainDelta) map[string]time.Time {
		t.Helper()
		news := map[string]time.Time{}
		if err := st.upsertDomains(ctx, ds, func(_ context.Context, _ *gorm.DB, ns []domainDelta) error {
			for _, d := range ns {
				news[d.Host] = d.FirstSeen
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return news
	}
	if got := upsert(delta("a.test", 2, t0)); len(got) != 1 || !got["a.test"].Equal(t0) {
		t.Fatalf("first batch: %v", got)
	}
	t1 := t0.Add(time.Minute)
	got := upsert(delta("b.test", 1, t1), delta("a.test", 3, t1), delta("c.test", 4, t1))
	if len(got) != 2 || !got["b.test"].Equal(t1) || !got["c.test"].Equal(t1) {
		t.Fatalf("second batch: %v", got)
	}
	var a model.PluginEgressDomain
	if err := store.G(ctx, db.Gorm).Where("plugin_key = ? AND host = ?", key, "a.test").Take(&a).Error; err != nil {
		t.Fatal(err)
	}
	if a.Connections != 5 || !a.FirstSeenAt.Equal(t0) || !a.LastSeenAt.Equal(t1) {
		t.Fatalf("a.test after two batches: %+v", a)
	}
	// More hosts than one statement's 65535 parameters hold (5 each).
	many := make([]domainDelta, 14_000)
	for i := range many {
		many[i] = delta(fmt.Sprintf("h%05d.test", i), 1, t1)
	}
	if got := upsert(many...); len(got) != len(many) {
		t.Fatalf("%d new hosts of %d", len(got), len(many))
	}
}

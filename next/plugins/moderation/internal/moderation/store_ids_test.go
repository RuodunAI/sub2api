package moderation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// TestWriteEventsIDs: every record gets the id of its own row, ascending in
// batch order, across the statements of a batch larger than one INSERT.
func TestWriteEventsIDs(t *testing.T) {
	p, _, _ := startDB(t, nil)
	ctx := context.Background()
	evs := make([]eventRec, pgdb.MaxBatchRows+3)
	at := time.Now().UTC()
	for i := range evs {
		evs[i] = eventRec{At: at, RequestID: fmt.Sprintf("ids-%04d", i), UserID: int64(i + 1), Mode: ModeObserve}
	}
	ids, err := p.writeEvents(ctx, evs)
	if err != nil {
		t.Fatal(err)
	}
	db, err := p.db(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID        int64
		RequestID string
		UserID    int64
	}
	if err := db.Raw(`SELECT id, request_id, user_id FROM events WHERE request_id LIKE 'ids-%' ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(evs) || len(rows) != len(evs) {
		t.Fatalf("%d ids, %d rows for %d records", len(ids), len(rows), len(evs))
	}
	for i, r := range rows {
		if r.ID != ids[i] || r.RequestID != evs[i].RequestID || r.UserID != evs[i].UserID {
			t.Fatalf("record %d (%s) got id %d; row %+v", i, evs[i].RequestID, ids[i], r)
		}
	}
}

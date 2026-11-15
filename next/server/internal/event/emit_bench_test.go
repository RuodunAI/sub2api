package event_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Emit runs inside the settler's transaction, under the user's balance lock
// when it carries a ledger change (usage insertAtomic), so its insert is on
// the request path. One transaction holds every iteration, rolled back at
// the end; n is the number of events of one call (internal/store/gormtest/
// BASELINE.md).
func BenchmarkHotEmit(b *testing.B) {
	db := testutil.DB(b)
	ctx := context.Background()
	pub := event.NewPublisher(db)
	for _, n := range []int{1, 2, 10} {
		events := make([]core.Event, n)
		for i := range events {
			events[i] = core.Event{Type: core.EventBalanceChanged, Payload: map[string]any{"user_id": 7, "ledger_id": i, "kind": "usage"}}
		}
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			tx, err := db.Pool.Begin(ctx)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := pub.Emit(ctx, tx, events...); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := pub.Emit(ctx, tx, events...); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

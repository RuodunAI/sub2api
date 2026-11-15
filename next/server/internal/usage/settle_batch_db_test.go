package usage

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// The usage_logs insert of a settler batch is one multi-row INSERT per
// power-of-two chunk. A batch of 203 records (chunks of 128, 64, 8, 2 and
// 1) holds a request id already stored, a repeat inside one chunk, a repeat
// in a later chunk and two ids that differ only past the 64 characters the
// column keeps: each of them is skipped, the first record of an id is the
// one inserted, and each inserted record gets its own row's id.
func TestInsertBatchChunksAndDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stored := f.record("batch-stored", false)
	if _, err := f.svc.insert(ctx, []*core.UsageRecord{stored}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 64)
	var batch []*core.UsageRecord
	for i := range 198 {
		rec := f.record(fmt.Sprintf("batch-%03d", i), i%3 != 0)
		rec.Tokens.Input = int64(i)
		batch = append(batch, rec)
	}
	// Index 3 repeats index 2 (pending) in the first chunk, as a free record.
	batch = slices.Insert(batch, 3, f.record("batch-002", false))
	batch = append(batch,
		f.record("batch-005", false),   // 199, in the chunk of 192..199
		f.record("batch-stored", true), // 200, in the chunk of 200..201
		f.record(long+"-a", false),     // 201
		f.record(long+"-b", true),      // 202, the last chunk: 201's row
	)
	if len(batch) != 203 {
		t.Fatalf("batch of %d", len(batch))
	}
	var ids map[string]int64
	inserted, err := f.svc.insertAtomic(ctx, batch, nil, func(_ pgx.Tx, got map[string]int64) error {
		ids = got
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[*core.UsageRecord]bool{batch[3]: true, batch[199]: true, batch[200]: true, batch[202]: true}
	var want []*core.UsageRecord
	for _, rec := range batch {
		if !skipped[rec] {
			want = append(want, rec)
		}
	}
	if len(inserted) != len(want) {
		t.Fatalf("inserted %d records, want %d", len(inserted), len(want))
	}
	for i, rec := range want {
		if inserted[i] != rec {
			t.Fatalf("inserted[%d] is %s, want %s", i, inserted[i].RequestID, rec.RequestID)
		}
		// The id is the row of this record: its own request id and tokens.
		// A native request's conversion is NULL, its documents are stored.
		row := f.scalar(`SELECT request_id || ' ' || input_tokens || ' ' || billing_status || ' ' || (conversion IS NULL)
			|| ' ' || plugin_detail::text || ' ' || jsonb_array_length(sched_decisions) FROM usage_logs WHERE id = $1`, ids[rec.RequestID])
		if want := trunc(rec.RequestID, 64) + " " + fmt.Sprint(rec.Tokens.Input) + " " + initialStatus(rec) + " true {} 1"; row != want {
			t.Fatalf("row of %s: %q, want %q", rec.RequestID, row, want)
		}
	}
	if n := f.scalar(`SELECT count(*) FROM usage_logs`); n != fmt.Sprint(len(want)+1) {
		t.Fatalf("%s rows", n)
	}
	// A repeat never overwrote the first record's row.
	if s := f.scalar(`SELECT billing_status FROM usage_logs WHERE request_id = 'batch-002'`); s != StatusPending {
		t.Fatalf("batch-002 is %s", s)
	}
	free := 0
	for _, rec := range want {
		if initialStatus(rec) == StatusFree {
			free++
		}
	}
	// One usage.recorded per free record inserted (the stored one included).
	if n := f.scalar(`SELECT count(*) FROM events WHERE type = 'usage.recorded'`); n != fmt.Sprint(free+1) {
		t.Fatalf("%s usage.recorded events, want %d", n, free+1)
	}
}

// A pending row settled by several workers at once (a retry racing the
// queue) is charged once, and the precharge it held comes back once.
func TestSettleConcurrentSameRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.record("race-row", true)
	if err := f.bill.Precharge(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.insert(ctx, []*core.UsageRecord{rec}); err != nil {
		t.Fatal(err)
	}
	p := fromRecord(rec)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.svc.settle(ctx, p, i%2 == 1) // the retry loop skips locked rows
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if s := f.scalar(`SELECT billing_status || ' ' || total_cost::text FROM usage_logs WHERE request_id = 'race-row'`); s != "billed 0.70800000" {
		t.Fatalf("row: %s", s)
	}
	for key, want := range map[string]string{"usage:race-row": "1", "precharge_release:race-row": "1", "precharge:race-row": "1"} {
		if n := f.scalar(`SELECT count(*) FROM balance_ledger WHERE idempotency_key = $1`, key); n != want {
			t.Fatalf("%s: %s rows", key, n)
		}
	}
	// 10 - 0.708: the precharge went out and came back once.
	if b := f.balance(); !b.Equal(dec("9.292")) {
		t.Fatalf("balance %s", b)
	}
	if s := f.scalar(`SELECT (SELECT sum(delta) FROM balance_ledger WHERE user_id = $1) = (SELECT balance FROM user_balances WHERE user_id = $1)`, f.user); s != "true" {
		t.Fatal("balance is not the ledger's sum")
	}
}

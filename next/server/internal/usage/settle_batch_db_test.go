package usage

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The usage_logs insert of a settler batch is one statement (insertUsageSQL,
// one array per column). A batch of 203 records holds a request id already
// stored, a repeat next to its first record, a repeat far from it and two
// ids that differ only past the 64 characters the column keeps: each of
// them is skipped, the first record of an id is the one inserted, and each
// inserted record gets its own row's id. (Batch 5a cut the batch into
// power-of-two chunks, 128, 64, 8, 2 and 1 here, and placed the repeats on
// both sides of the chunk boundaries.)
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
	inserted, err := f.svc.insertAtomic(ctx, batch, nil, func(_ *gorm.DB, got map[string]int64) error {
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

// The batch insert stores each value as the one-row VALUES insert of the
// pgx batch it replaced did, column by column: a nanosecond timestamp, a
// rate past the column's scale, NULL and non-NULL account and conversion,
// documents, free and billable records.
func TestInsertBatchMatchesValuesInsert(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account := int64(42)
	full := func(rec *core.UsageRecord) *core.UsageRecord {
		rec.AccountID, rec.Stream, rec.StickyRule, rec.StickyHit = &account, true, "rule", true
		rec.ErrorType, rec.ErrorMessage, rec.Attempts = "upstream", "boom ü", 3
		rec.LatencyMs, rec.FirstTokenMs, rec.UploadMs = 1234, 56, 7
		rec.ClientIP, rec.UserAgent, rec.NodeID, rec.ClientRequestID = "203.0.113.9", "ua/1", "node-1", "client-1"
		rec.RateMultiplier = decimal.RequireFromString("1.23456")
		rec.CreatedAt = time.Date(2026, 11, 15, 12, 0, 0, 123456789, time.UTC)
		rec.Metrics = map[string]any{"seconds": 8.5}
		rec.PluginDetail = core.RawJSON(`{"job":"j-1"}`)
		rec.Conversion = &core.ConversionTrace{V: 1, Grade: "lossy", Hops: []string{"a", "b"}}
		return rec
	}
	variants := map[string]func(id string) *core.UsageRecord{
		"billable": func(id string) *core.UsageRecord { return full(f.record(id, true)) },
		"free":     func(id string) *core.UsageRecord { return f.record(id, false) },
	}
	columns := make([]string, len(usageLogColumns))
	params := make([]string, len(usageLogColumns))
	for i, c := range usageLogColumns {
		columns[i], params[i] = c[0], fmt.Sprintf("$%d", i+1)
	}
	valuesSQL := `INSERT INTO usage_logs (` + strings.Join(columns, ", ") + `) VALUES (` + strings.Join(params, ", ") + `)
		ON CONFLICT (request_id) DO NOTHING RETURNING id`
	var batch []*core.UsageRecord
	for name, build := range variants {
		var id int64
		if err := f.db.SQL.QueryRowContext(ctx, valuesSQL, appendUsageRow(nil, build("values-"+name))...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, build("unnest-"+name))
		// A batch of one takes insertUsageRowSQL.
		if _, err := f.svc.insert(ctx, []*core.UsageRecord{build("row-" + name)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.insert(ctx, batch); err != nil {
		t.Fatal(err)
	}
	const row = `SELECT (to_jsonb(u) - 'id' - 'request_id')::text FROM usage_logs u WHERE request_id = $1`
	for name := range variants {
		want := f.scalar(row, "values-"+name)
		for _, form := range []string{"unnest-", "row-"} {
			if got := f.scalar(row, form+name); got != want {
				t.Fatalf("%s%s:\n got %s\nwant %s", form, name, got, want)
			}
		}
	}
}

// countingQuerier counts the statements run through it.
type countingQuerier struct {
	*gorm.DB
	n *int
}

func (c countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	*c.n++
	return c.Querier.Exec(ctx, sql, args...)
}

func (c countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	*c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	*c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// Any batch is one statement, one round trip, as the pgx batch was (batch
// 5a took one per power-of-two chunk: 300 is 128, 128, 32, 8 and 4).
func TestInsertBatchOneStatement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	batch := make([]*core.UsageRecord, 300)
	for i := range batch {
		batch[i] = f.record(fmt.Sprintf("one-%03d", i), i%2 == 0)
	}
	var statements int
	err := f.db.Tx(ctx, func(tx *gorm.DB) error {
		ids, err := insertUsageRows(store.G(ctx, countingQuerier{Querier: tx, n: &statements}), batch)
		for i, id := range ids {
			if id == 0 {
				t.Errorf("record %d not inserted", i)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if statements != 1 {
		t.Fatalf("%d statements for a batch of %d", statements, len(batch))
	}
}

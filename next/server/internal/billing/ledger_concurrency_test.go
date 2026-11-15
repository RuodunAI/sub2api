package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// The ledger's concurrency and idempotency contracts against a real
// PostgreSQL: whatever interleaving the balance lock and the unique
// idempotency keys allow, each key moves money once, every replay reports
// the row that moved it, and the balance is the sum of the ledger.

// concurrently runs fn(i) for i in [0, n) at once and returns the errors.
func concurrently(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			errs[i] = fn(i)
		}()
	}
	start.Done()
	done.Wait()
	return errs
}

// assertLedgerBalanced checks that the user's balance is the sum of the
// user's ledger rows and equals want.
func (e *env) assertLedgerBalanced(uid int64, want string) {
	e.t.Helper()
	var sum decimal.Decimal
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT COALESCE(sum(delta), 0) FROM balance_ledger WHERE user_id = $1`, uid).Scan(&sum); err != nil {
		e.t.Fatal(err)
	}
	bal := e.balance(uid)
	if !bal.Equal(sum) || !bal.Equal(decimal.RequireFromString(want)) {
		e.t.Fatalf("balance %s, ledger sum %s, want %s", bal, sum, want)
	}
}

// Every request is settled three times at once (a retry racing the first
// settlement): one refund and one charge per request, the replays report
// the charge as a duplicate, and the balance is the ledger's sum.
func TestSettleConcurrentReplays(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.user("settle-race@test.invalid")
	e.seed(uid, "100")
	const requests, replays = 6, 3
	for r := range requests {
		if err := e.svc.Precharge(ctx, usageRecord(uid, fmt.Sprintf("race-%d", r), "flat(2)")); err != nil {
			t.Fatal(err)
		}
	}
	e.assertLedgerBalanced(uid, "88")
	results := make([]*core.LedgerResult, requests*replays)
	errs := concurrently(requests*replays, func(i int) error {
		var err error
		results[i], err = e.settle(uid, fmt.Sprintf("race-%d", i%requests), "1.25")
		return err
	})
	for r := range requests {
		fresh, ledgerID := 0, int64(0)
		for k := range replays {
			i := k*requests + r
			if errs[i] != nil {
				t.Fatalf("settlement %d: %v", i, errs[i])
			}
			res := results[i]
			if res == nil || res.LedgerID == 0 || (ledgerID != 0 && res.LedgerID != ledgerID) {
				t.Fatalf("request %d replay %d: %+v (charge %d)", r, k, res, ledgerID)
			}
			ledgerID = res.LedgerID
			if !res.Duplicate {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("request %d charged %d times", r, fresh)
		}
	}
	if n := e.count(`SELECT count(*) FROM balance_ledger WHERE user_id = $1 AND idempotency_key LIKE 'usage:race-%'`, uid); n != requests {
		t.Fatalf("%d charges", n)
	}
	if n := e.count(`SELECT count(*) FROM balance_ledger WHERE user_id = $1 AND idempotency_key LIKE 'precharge_release:race-%'`, uid); n != requests {
		t.Fatalf("%d refunds", n)
	}
	if n := e.count(`SELECT count(*) FROM request_precharges WHERE user_id = $1`, uid); n != 0 {
		t.Fatalf("%d reservations left", n)
	}
	// 100 - 6 × 1.25: the reservations came back, the charges went out once.
	e.assertLedgerBalanced(uid, "92.5")
	// seed, 6 precharges, 6 refunds, 6 charges: replays emit nothing.
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'balance.changed' AND payload->>'user_id' = $1`, itoa(uid)); n != 1+3*requests {
		t.Fatalf("%d balance events", n)
	}
}

// One idempotency key applied by many callers at once moves the balance
// once; the others get that row back as a duplicate, and a different
// amount under the key is a conflict.
func TestApplyConcurrentSameKey(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.user("apply-race@test.invalid")
	ch := core.LedgerChange{UserID: uid, Amount: decimal.RequireFromString("3.5"), Credit: true, Kind: KindAdminAdjust, IdempotencyKey: "race:apply"}
	const callers = 8
	results := make([]*core.LedgerResult, callers)
	errs := concurrently(callers, func(i int) error {
		var err error
		results[i], err = e.svc.Apply(ctx, ch)
		return err
	})
	fresh := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].LedgerID != results[0].LedgerID || !results[i].BalanceAfter.Equal(decimal.RequireFromString("3.5")) {
			t.Fatalf("caller %d: %+v, caller 0: %+v", i, results[i], results[0])
		}
		if !results[i].Duplicate {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("applied %d times", fresh)
	}
	e.assertLedgerBalanced(uid, "3.5")
	ch.Amount = decimal.NewFromInt(4)
	if _, err := e.svc.Apply(ctx, ch); core.AsError(err).Code != core.ErrConflict.Code {
		t.Fatalf("reused key: %v", err)
	}
}

// A precharge replayed concurrently (the gateway retrying one request id)
// reserves and charges once and answers success to every caller. Then
// precharges of other requests for the same user queue behind the balance
// lock and are admitted until the balance runs out, while further replays
// still succeed without charging.
func TestPrechargeConcurrentReplayAndAdmission(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	uid := e.user("precharge-race@test.invalid")
	e.seed(uid, "5")
	const replays, others = 6, 6
	same := func(int) error { return e.svc.Precharge(ctx, usageRecord(uid, "same", "flat(1)")) }
	for i, err := range concurrently(replays, same) {
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	e.assertLedgerBalanced(uid, "4")
	errs := concurrently(replays+others, func(i int) error {
		if i < replays {
			return same(i)
		}
		return e.svc.Precharge(ctx, usageRecord(uid, fmt.Sprintf("other-%d", i), "flat(1)"))
	})
	admitted := 0
	for i, err := range errs {
		switch {
		case i < replays && err != nil:
			t.Fatalf("late replay %d: %v", i, err)
		case err == nil && i >= replays:
			admitted++
		case err != nil && core.AsError(err).Code != core.ErrInsufficientBalance.Code:
			t.Fatalf("precharge %d: %v", i, err)
		}
	}
	// 4 leaves room for three more of 1 above the 0 minimum (the balance
	// after a precharge must stay above it).
	if admitted != 3 {
		t.Fatalf("admitted %d other requests", admitted)
	}
	if n := e.count(`SELECT count(*) FROM request_precharges WHERE user_id = $1 AND request_id = 'same'`, uid); n != 1 {
		t.Fatalf("%d reservations of the replayed request", n)
	}
	if n := e.count(`SELECT count(*) FROM balance_ledger WHERE idempotency_key = 'precharge:same'`); n != 1 {
		t.Fatalf("%d ledger rows of the replayed request", n)
	}
	e.assertLedgerBalanced(uid, "1")
}

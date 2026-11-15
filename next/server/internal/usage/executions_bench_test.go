package usage

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The plugin_executions statements of an Execute plugin run for every
// request it serves: the intent and the observation once per attempt, the
// lock and the commit mark inside the usage transaction. They stay on pgx;
// the GORM form of each statement is measured next to it, against a real
// PostgreSQL (TEST_DATABASE_URL), as recorded in
// internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench HotExecution -benchmem -benchtime 1000x -count 5 ./internal/usage/
func BenchmarkHotExecution(b *testing.B) {
	db := testutil.DB(b)
	s := &Service{db: db}
	ctx := context.Background()
	rec := &core.UsageRecord{PluginKey: "bench", Model: "m", Endpoint: "/v1/messages"}
	account := int64(7)
	rec.AccountID = &account
	observation := []byte("bounded observation")
	n := 0
	// next is a fresh execution id; intent and observed prepare it in the
	// state the measured statement expects, untimed.
	next := func() string { n++; return "bench-exec-" + strconv.Itoa(n) }
	intent := func(b *testing.B, id string) {
		rec.RequestID = id
		if err := s.BeginExecution(ctx, core.ExecutionIntent{ID: id, RequestID: id, PluginKey: rec.PluginKey, AccountID: account, Attempt: 1, Record: rec}); err != nil {
			b.Fatal(err)
		}
	}
	observed := func(b *testing.B, id string) {
		intent(b, id)
		if err := s.ObserveExecution(ctx, id, rec, observation); err != nil {
			b.Fatal(err)
		}
	}
	inTx := func(fn func(pgx.Tx) error) error { return db.Tx(ctx, fn) }
	cases := []struct {
		name    string
		prepare func(*testing.B, string)
		run     func(id string) error
	}{
		{"begin/pgx", nil, func(id string) error {
			rec.RequestID = id
			return s.BeginExecution(ctx, core.ExecutionIntent{ID: id, RequestID: id, PluginKey: rec.PluginKey, AccountID: account, Attempt: 1, Record: rec})
		}},
		{"begin/gorm", nil, func(id string) error {
			rec.RequestID = id
			return store.G(ctx, db.Pool).Create(&model.PluginExecution{ID: id, RequestID: id, PluginKey: rec.PluginKey, AccountID: account,
				Attempt: 1, Record: jsonOr(rec, "{}")}).Error
		}},
		{"observe/pgx", intent, func(id string) error { return s.ObserveExecution(ctx, id, rec, observation) }},
		{"observe/gorm", intent, func(id string) error {
			raw, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			return store.G(ctx, db.Pool).Model(&model.PluginExecution{}).
				Where("id = ? AND state = 'intent' AND request_id = ? AND account_id = ?", id, rec.RequestID, rec.AccountID).
				Updates(map[string]any{"state": "observed", "record": raw, "observation": observation, "updated_at": gorm.Expr("clock_timestamp()")}).Error
		}},
		{"commit/pgx", observed, func(id string) error {
			return inTx(func(tx pgx.Tx) error {
				if _, err := lockExecution(ctx, tx, id); err != nil {
					return err
				}
				return markExecutionCommitted(ctx, tx, id, "usage", "digest", "")
			})
		}},
		{"commit/gorm", observed, func(id string) error {
			return inTx(func(tx pgx.Tx) error {
				g := store.G(ctx, tx)
				var x lockedExecution
				if err := g.Model(&model.PluginExecution{}).Select("state, operation, digest, task_id, request_id, plugin_key, account_id").
					Where("id = ?", id).Clauses(store.ForUpdate).Row().
					Scan(&x.state, &x.op, &x.digest, &x.taskID, &x.requestID, &x.pluginKey, &x.accountID); err != nil {
					return err
				}
				return g.Model(&model.PluginExecution{}).Where("id = ?", id).Updates(map[string]any{"state": "committed", "operation": "usage",
					"digest": "digest", "task_id": "", "record": nil, "observation": nil, "updated_at": gorm.Expr("clock_timestamp()")}).Error
			})
		}},
	}
	for _, bc := range cases {
		b.Run(bc.name, func(b *testing.B) {
			step := func() {
				id := next()
				if bc.prepare != nil {
					b.StopTimer()
					bc.prepare(b, id)
					b.StartTimer()
				}
				if err := bc.run(id); err != nil {
					b.Fatal(err)
				}
			}
			step() // the first run prepares the statements
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				step()
			}
		})
	}
}

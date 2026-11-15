package usage

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The plugin_executions statements of an Execute plugin run for every
// request it serves: the intent and the observation once per attempt, the
// lock and the commit mark inside the usage transaction. They run on GORM
// (gorm: the production functions); the pgx form each had before is
// measured next to it, against a real PostgreSQL (TEST_DATABASE_URL), as
// recorded in internal/store/gormtest/BASELINE.md:
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
	inTx := func(fn func(*gorm.DB) error) error { return db.Tx(ctx, fn) }
	cases := []struct {
		name    string
		prepare func(*testing.B, string)
		run     func(id string) error
	}{
		{"begin/pgx", nil, func(id string) error {
			rec.RequestID = id
			_, err := db.SQL.ExecContext(ctx, `INSERT INTO plugin_executions(id,request_id,plugin_key,account_id,attempt,record,task_submit)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, id, id, rec.PluginKey, account, 1, jsonOr(rec, "{}"), false)
			return err
		}},
		{"begin/gorm", nil, func(id string) error {
			rec.RequestID = id
			return s.BeginExecution(ctx, core.ExecutionIntent{ID: id, RequestID: id, PluginKey: rec.PluginKey, AccountID: account, Attempt: 1, Record: rec})
		}},
		{"observe/pgx", intent, func(id string) error {
			raw, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			_, err = db.SQL.ExecContext(ctx, `UPDATE plugin_executions SET state='observed',record=$2,observation=$3,updated_at=clock_timestamp()
 WHERE id=$1 AND state='intent' AND request_id=$4 AND account_id=$5`, id, raw, observation, rec.RequestID, rec.AccountID)
			return err
		}},
		{"observe/gorm", intent, func(id string) error { return s.ObserveExecution(ctx, id, rec, observation) }},
		{"commit/pgx", observed, func(id string) error {
			return inTx(func(tx *gorm.DB) error {
				var x lockedExecution
				if err := store.SQLTx(tx).QueryRowContext(ctx, `SELECT state,operation,digest,task_id,request_id,plugin_key,account_id FROM plugin_executions WHERE id=$1 FOR UPDATE`, id).
					Scan(&x.state, &x.op, &x.digest, &x.taskID, &x.requestID, &x.pluginKey, &x.accountID); err != nil {
					return err
				}
				_, err := store.SQLTx(tx).ExecContext(ctx, `UPDATE plugin_executions SET state='committed',operation=$2,digest=$3,task_id=$4,record=NULL,observation=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, "usage", "digest", "")
				return err
			})
		}},
		{"commit/gorm", observed, func(id string) error {
			return inTx(func(tx *gorm.DB) error {
				if _, err := lockExecution(ctx, tx, id); err != nil {
					return err
				}
				return markExecutionCommitted(ctx, tx, id, "usage", "digest", "")
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

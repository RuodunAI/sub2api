package usage

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// The statements of the async task endpoints, which the gateway runs for
// every task submission (BeginTask, ReceiveTask, RegisterTask) and every
// poll of a client (FindTask). Each production statement, on GORM (gorm),
// is measured next to the pgx form it had before (pgx) against a real
// PostgreSQL (TEST_DATABASE_URL), as recorded in
// internal/store/gormtest/BASELINE.md (Batch 4b):
//
//	go test -run '^$' -bench HotTask -benchmem -benchtime 1000x -count 5 ./internal/usage/
//
// RegisterTask is measured as the statements of recordTask, in their own
// transaction; the usage insert around them is BenchmarkHotSettlerInsert*.

// taskFixture is a registered async task and its receipt.
type taskFixture struct {
	*benchSettler
	intent core.TaskIntent
	query  core.TaskQuery
	n      int
}

func newTaskFixture(b *testing.B) *taskFixture {
	f := &taskFixture{benchSettler: newBenchSettler(b)}
	ctx := context.Background()
	reg := f.submission(b)
	if err := f.svc.db.Tx(ctx, func(tx pgx.Tx) error {
		return recordTask(ctx, tx, reg.TaskRegistration, reg.usageID, time.Now().Add(time.Hour), time.Now().Add(24*time.Hour))
	}); err != nil {
		b.Fatal(err)
	}
	f.intent = reg.intent
	f.query = core.TaskQuery{PluginKey: reg.intent.PluginKey, Kind: reg.intent.Kind, UserID: f.user, GroupID: f.group, ID: reg.PublicID}
	return f
}

// taskSubmission is a submitted task not yet registered: its usage row,
// its receipt (received) and the settlement entry of its reservation.
type taskSubmission struct {
	core.TaskRegistration
	intent  core.TaskIntent
	usageID int64
}

func (f *taskFixture) submission(b *testing.B) taskSubmission {
	ctx := context.Background()
	f.n++
	publicID := "s2task_bench_" + strconv.Itoa(f.n)
	rec := f.record(1000+f.n, true)
	account := int64(7)
	rec.AccountID = &account
	f.insert(b, rec)
	s := taskSubmission{
		TaskRegistration: core.TaskRegistration{PublicID: publicID, UpstreamID: "up-" + publicID, Kind: "video",
			Snapshot: []byte(`{"id":"` + publicID + `","status":"queued"}`), IDPaths: []string{"id"}, Record: rec},
		intent: core.TaskIntent{RequestID: rec.RequestID, PluginKey: rec.PluginKey, Kind: "video", UserID: rec.UserID,
			APIKeyID: rec.APIKeyID, GroupID: rec.GroupID, AccountID: account, Model: rec.Model, Protocol: rec.Protocol},
	}
	if err := f.svc.db.Pool.QueryRow(ctx, `SELECT id FROM usage_logs WHERE request_id = $1`, rec.RequestID).Scan(&s.usageID); err != nil {
		b.Fatal(err)
	}
	if err := insertReceipt(ctx, f.svc.db.Pool, publicID, s.intent); err != nil {
		b.Fatal(err)
	}
	if err := receiveTask(ctx, f.svc.db.Pool, rec.RequestID, "received", 200, s.Snapshot, ""); err != nil {
		b.Fatal(err)
	}
	if _, err := f.svc.db.Pool.Exec(ctx, `INSERT INTO pending_settlements (plugin_key, ref_id, usage_log_id, account_id, next_check_at, deadline_at)
		VALUES ($1, $2, $3, $4, now(), now() + interval '1 day')`, rec.PluginKey, publicID, s.usageID, account); err != nil {
		b.Fatal(err)
	}
	return s
}

// benchTask runs the pgx and the GORM form of one statement; prepare, if
// set, runs untimed before each call and returns what the call takes.
func benchTask[T any](b *testing.B, prepare func(*testing.B) T, pgxPath, gormPath func(T) error) {
	for _, p := range []struct {
		name string
		run  func(T) error
	}{{"pgx", pgxPath}, {"gorm", gormPath}} {
		b.Run(p.name, func(b *testing.B) {
			step := func() {
				var arg T
				if prepare != nil {
					b.StopTimer()
					arg = prepare(b)
					b.StartTimer()
				}
				if err := p.run(arg); err != nil {
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

// FindTask's read of a task by its public ID.
func BenchmarkHotTaskFind(b *testing.B) {
	f := newTaskFixture(b)
	ctx := context.Background()
	q, pool := f.query, f.svc.db.Pool
	benchTask(b, nil, func(struct{}) error {
		rows, err := pool.Query(ctx, `SELECT public_id,upstream_ref_id,model,state,
			CASE WHEN state='pending' AND deadline_at<=clock_timestamp() THEN 'abandoned' ELSE observation_status END,
			snapshot,snapshot_id_paths,failure_code,last_error
			FROM async_tasks WHERE plugin_key=$1 AND kind=$2 AND user_id=$3 AND group_id=$4
			AND (public_id=$5 OR (legacy_imported AND upstream_ref_id=$5 AND NOT starts_with($5,'s2task_')))
			ORDER BY public_id LIMIT 2`, q.PluginKey, q.Kind, q.UserID, q.GroupID, q.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		_, err = scanTask(rows)
		return err
	}, func(struct{}) error {
		_, err := readTask(ctx, pool, q)
		return err
	})
}

// BeginTask's receipt, a new request ID each time.
func BenchmarkHotTaskBegin(b *testing.B) {
	f := newTaskFixture(b)
	ctx := context.Background()
	pool := f.svc.db.Pool
	next := func(*testing.B) core.TaskIntent {
		f.n++
		in := f.intent
		in.RequestID = "bench-begin-" + strconv.Itoa(f.n)
		return in
	}
	benchTask(b, next, func(in core.TaskIntent) error {
		_, err := pool.Exec(ctx, `INSERT INTO task_submission_receipts
		(request_id, public_id, plugin_key, kind, user_id, api_key_id, group_id, account_id, model, protocol)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, in.RequestID, "s2task_"+in.RequestID, in.PluginKey, in.Kind,
			in.UserID, in.APIKeyID, in.GroupID, in.AccountID, in.Model, in.Protocol)
		return err
	}, func(in core.TaskIntent) error {
		return insertReceipt(ctx, pool, "s2task_"+in.RequestID, in)
	})
}

// ReceiveTask's write of the upstream answer, on a new receipt (being
// dispatched) each time.
func BenchmarkHotTaskReceive(b *testing.B) {
	f := newTaskFixture(b)
	ctx := context.Background()
	pool := f.svc.db.Pool
	body := []byte(`{"id":"up-bench","status":"queued"}`)
	dispatched := func(b *testing.B) string {
		f.n++
		in := f.intent
		in.RequestID = "bench-receive-" + strconv.Itoa(f.n)
		if err := insertReceipt(ctx, pool, "s2task_"+in.RequestID, in); err != nil {
			b.Fatal(err)
		}
		return in.RequestID
	}
	benchTask(b, dispatched, func(id string) error {
		_, err := pool.Exec(ctx, `UPDATE task_submission_receipts SET state=$2, response_status=$3,
		response_body=COALESCE($4,response_body), last_error=$5 WHERE request_id=$1 AND state <> 'registered'`,
			id, "received", 200, body, "")
		return err
	}, func(id string) error {
		return receiveTask(ctx, pool, id, "received", 200, body, "")
	})
}

// RegisterTask's statements (recordTask), in a transaction of their own,
// for a new submission each time.
func BenchmarkHotTaskRegister(b *testing.B) {
	f := newTaskFixture(b)
	ctx := context.Background()
	next, deadline := time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)
	benchTask(b, f.submission, func(s taskSubmission) error {
		return f.svc.db.Tx(ctx, func(tx pgx.Tx) error { return recordTaskPGX(ctx, tx, s.TaskRegistration, s.usageID, next, deadline) })
	}, func(s taskSubmission) error {
		return f.svc.db.Tx(ctx, func(tx pgx.Tx) error { return recordTask(ctx, tx, s.TaskRegistration, s.usageID, next, deadline) })
	})
}

// recordTaskPGX is recordTask's statements in the pgx form they had before.
func recordTaskPGX(ctx context.Context, tx pgx.Tx, in core.TaskRegistration, uid int64, next, deadline time.Time) error {
	r := in.Record
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT public_id=$2 AND plugin_key=$3 AND kind=$4 AND user_id=$5
		AND api_key_id=$6 AND group_id=$7 AND account_id=$8 AND model=$9 AND protocol=$10
		FROM task_submission_receipts WHERE request_id=$1 AND state='received' FOR UPDATE`,
		r.RequestID, in.PublicID, r.PluginKey, in.Kind, r.UserID, r.APIKeyID, r.GroupID, *r.AccountID, r.Model, r.Protocol).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("task receipt identity mismatch")
	}
	_, err := tx.Exec(ctx, `INSERT INTO async_tasks(public_id,plugin_key,kind,upstream_ref_id,user_id,api_key_id,
		group_id,account_id,model,protocol,usage_log_id,snapshot,snapshot_id_paths,next_check_at,deadline_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, in.PublicID, r.PluginKey, in.Kind, in.UpstreamID,
		r.UserID, r.APIKeyID, r.GroupID, *r.AccountID, r.Model, r.Protocol, uid, in.Snapshot, jsonOr(in.IDPaths, "[]"), next, deadline)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE pending_settlements SET task_public_id=$2 WHERE usage_log_id=$1`, uid, in.PublicID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE task_submission_receipts SET state='registered',response_body=NULL,last_error='',
		expires_at=LEAST(expires_at,$2) WHERE request_id=$1`, r.RequestID, deadline)
	return err
}

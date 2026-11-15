package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var errExecutionDone = errors.New("execution already committed")

func (s *Service) BeginExecution(ctx context.Context, in core.ExecutionIntent) error {
	if in.ID == "" || in.RequestID == "" || in.AccountID == 0 {
		return errors.New("invalid execution identity")
	}
	return store.G(ctx, s.db.Gorm).Exec(`INSERT INTO plugin_executions(id,request_id,plugin_key,account_id,attempt,record,task_submit)
 VALUES(?,?,?,?,?,?,?)`, in.ID, in.RequestID, in.PluginKey, in.AccountID, in.Attempt, store.Bytes(jsonOr(in.Record, "{}")), in.TaskSubmit).Error
}

func (s *Service) ObserveExecution(ctx context.Context, id string, rec *core.UsageRecord, observation []byte) error {
	if rec == nil || len(observation) > 512<<10 {
		return errors.New("invalid execution observation")
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if len(raw) > 512<<10 {
		return errors.New("execution facts exceed limit")
	}
	res := store.G(ctx, s.db.Gorm).Exec(`UPDATE plugin_executions SET state='observed',record=?,observation=?,updated_at=clock_timestamp()
 WHERE id=? AND state='intent' AND request_id=? AND account_id=?`, store.Bytes(raw), store.Bytes(observation), id, rec.RequestID, rec.AccountID)
	if res.Error == nil && res.RowsAffected != 1 {
		return errors.New("execution identity or state mismatch")
	}
	return res.Error
}

func (s *Service) CommitExecution(ctx context.Context, in core.ExecutionCommit) (*pluginv1.ExecutionReceipt, error) {
	if in.Record == nil || in.Record.AccountID == nil || len(in.Digest) != 64 || (in.Operation != "usage" && in.Operation != "watch") {
		return nil, errors.New("invalid execution commit")
	}
	out := &pluginv1.ExecutionReceipt{}
	before := func(tx *gorm.DB) error {
		x, err := lockExecution(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		out.TaskId = x.taskID
		if x.requestID != in.Record.RequestID || x.pluginKey != in.Record.PluginKey || x.accountID != *in.Record.AccountID {
			return errors.New("execution identity mismatch")
		}
		if x.state == "committed" {
			if x.op != in.Operation || x.digest != in.Digest {
				return core.ErrConflict.WithMessage("execution facts already committed")
			}
			return errExecutionDone
		}
		if x.state != "observed" {
			return errors.New("execution has no durable observation")
		}
		return nil
	}
	after := func(tx *gorm.DB) error {
		if in.Task != nil {
			out.TaskId = in.Task.PublicID
		}
		return markExecutionCommitted(ctx, tx, in.ID, in.Operation, in.Digest, out.TaskId)
	}
	var err error
	if in.Task != nil {
		if in.Operation != "watch" {
			return nil, errors.New("wrong execution operation")
		}
		in.Task.Record = in.Record
		err = s.registerTask(ctx, *in.Task, before, after, true)
	} else {
		if in.Operation != "usage" {
			return nil, errors.New("task registration required")
		}
		_, err = s.insertAtomic(ctx, []*core.UsageRecord{in.Record}, before, func(tx *gorm.DB, ids map[string]int64) error {
			if _, ok := ids[in.Record.RequestID]; !ok {
				return errors.New("usage identity already recorded")
			}
			return after(tx)
		}, true)
	}
	if errors.Is(err, errExecutionDone) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lockedExecution is the plugin_executions row CommitExecution locks.
type lockedExecution struct {
	state, op, digest, taskID, requestID, pluginKey string
	accountID                                       int64
}

// lockExecution locks and reads the execution id. It runs for every request
// of an Execute plugin (BenchmarkHotExecution, gormtest/BASELINE.md).
func lockExecution(ctx context.Context, tx *gorm.DB, id string) (lockedExecution, error) {
	var x lockedExecution
	err := store.G(ctx, tx).Raw(`SELECT state,operation,digest,task_id,request_id,plugin_key,account_id FROM plugin_executions WHERE id=? FOR UPDATE`, id).
		Row().Scan(&x.state, &x.op, &x.digest, &x.taskID, &x.requestID, &x.pluginKey, &x.accountID)
	return x, err
}

// markExecutionCommitted records the commit of the execution id and drops its
// facts, for every request of an Execute plugin.
func markExecutionCommitted(ctx context.Context, tx *gorm.DB, id, op, digest, taskID string) error {
	return store.G(ctx, tx).Exec(`UPDATE plugin_executions SET state='committed',operation=?,digest=?,task_id=?,record=NULL,observation=NULL,updated_at=clock_timestamp() WHERE id=?`, op, digest, taskID, id).Error
}

// Recovery never replays network I/O. Only completed host observations may
// fall back to declarative facts; unobserved intents remain uncertain.
func (s *Service) recoverExecutions(ctx context.Context) error {
	g := store.G(ctx, s.db.Gorm)
	var list []stalledExecution
	if err := stalledExecutions(g).Scan(&list).Error; err != nil {
		return err
	}
	for _, i := range list {
		var r core.UsageRecord
		if err := json.Unmarshal(i.Record, &r); err != nil {
			s.deferExecutionRecovery(ctx, i.ID, err)
			continue
		}
		// Tasks need their validated parser result; the durable submission
		// receipt remains available for repair instead of inventing an ID.
		task, err := store.Exists(g, g.Model(&model.TaskSubmissionReceipt{}).Where("request_id = ?", r.RequestID))
		if err != nil {
			return err
		}
		if task {
			continue
		}
		_, err = s.CommitExecution(ctx, core.ExecutionCommit{ID: i.ID, Operation: "usage", Digest: fmt.Sprintf("%064x", 0), Record: &r})
		if err != nil && !errors.Is(err, core.ErrConflict) {
			s.deferExecutionRecovery(ctx, i.ID, err)
		}
	}
	return s.cleanupExecutionReceipts(ctx)
}

// stalledExecution is a row of stalledExecutions.
type stalledExecution struct {
	ID     string `gorm:"column:id"`
	Record []byte `gorm:"column:record"`
}

// stalledExecutions selects the oldest observed executions that neither
// committed nor submitted a task within a minute.
func stalledExecutions(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT id,record FROM plugin_executions WHERE state='observed' AND NOT task_submit AND NOT EXISTS(SELECT 1 FROM task_submission_receipts t WHERE t.request_id=plugin_executions.request_id) AND updated_at<clock_timestamp()-interval '1 minute' ORDER BY updated_at LIMIT 100`)
}

func (s *Service) deferExecutionRecovery(ctx context.Context, id string, cause error) {
	slog.ErrorContext(ctx, "usage: execution recovery deferred", "execution_id", id, "err", cause)
	// Rotate failures behind other observations. A permanently unpriceable
	// record must not monopolize the oldest LIMIT batch on every sweep.
	_ = rotateExecution(store.G(ctx, s.db.Gorm), id).Error
}

// rotateExecution moves an observed execution to the back of the recovery
// order.
func rotateExecution(db *gorm.DB, id string) *gorm.DB {
	return db.Model(&model.PluginExecution{}).Where("id = ? AND state = 'observed'", id).Update("updated_at", gorm.Expr("clock_timestamp()"))
}

var _ core.ExecutionRecords = (*Service)(nil)

// Retention eligibility is seven days; bounded batches catch up without
// monopolizing the settlement retry worker or holding a long transaction.
func (s *Service) cleanupExecutionReceipts(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, table := range []string{"plugin_executions", "plugin_monitor_receipts"} {
		for range 20 {
			if ctx.Err() != nil {
				return nil
			}
			res := expireReceipts(store.G(ctx, s.db.Gorm), table)
			if err := res.Error; err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if res.RowsAffected < 1000 {
				break
			}
		}
	}
	return nil
}

// expireReceipts deletes a batch of up to 1000 rows of table (plugin_executions
// or plugin_monitor_receipts) older than seven days, the oldest first.
func expireReceipts(db *gorm.DB, table string) *gorm.DB {
	return db.Exec(`DELETE FROM ` + table + ` WHERE id IN(SELECT id FROM ` + table + ` WHERE created_at<clock_timestamp()-interval '7 days' ORDER BY created_at LIMIT 1000)`)
}

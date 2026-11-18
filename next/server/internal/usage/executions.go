package usage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var _ core.ExecutionRecords = (*Service)(nil)

// errExecutionDone ends the transaction of a repeated commit or close
// without writing anything; the caller reports success.
var errExecutionDone = errors.New("execution already ended")

func (s *Service) BeginExecution(ctx context.Context, in core.ExecutionIntent) error {
	if in.ID == "" || in.RequestID == "" || in.AccountID == 0 {
		return errors.New("invalid execution identity")
	}
	return store.G(ctx, s.db.Gorm).Create(&model.PluginExecution{ID: in.ID, RequestID: in.RequestID, PluginKey: in.PluginKey,
		AccountID: in.AccountID, Attempt: in.Attempt, Record: jsonOr(in.Record, "{}"), TaskSubmit: in.TaskSubmit}).Error
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
	res := execution(store.G(ctx, s.db.Gorm), id).Where("state=? AND request_id=? AND account_id=?", core.ExecutionStateIntent, rec.RequestID, rec.AccountID).
		Updates(map[string]any{"state": core.ExecutionStateObserved, "record": store.Bytes(raw), "observation": store.Bytes(observation),
			"updated_at": gorm.Expr("clock_timestamp()")})
	if res.Error == nil && res.RowsAffected != 1 {
		return errors.New("execution identity or state mismatch")
	}
	return res.Error
}

func (s *Service) CommitExecution(ctx context.Context, in core.ExecutionCommit) error {
	task := in.Operation == core.ExecutionOpWatch
	if in.Record == nil || in.Record.AccountID == nil || len(in.Digest) != 64 || (in.Task != nil) != task ||
		!task && in.Operation != core.ExecutionOpUsage {
		return errors.New("invalid execution commit")
	}
	before := func(tx *gorm.DB) error {
		x, err := lockExecution(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		if x.RequestID != in.Record.RequestID || x.PluginKey != in.Record.PluginKey || x.AccountID != *in.Record.AccountID {
			return errors.New("execution identity mismatch")
		}
		switch core.ValueOr(x.State, "") {
		case core.ExecutionStateObserved:
			return nil
		case core.ExecutionStateCommitted:
			if x.Operation == in.Operation && x.Digest == in.Digest {
				return errExecutionDone
			}
			return core.ErrConflict.WithMessage("execution facts already committed")
		case core.ExecutionStateUnreported:
			return core.ErrConflict.WithMessage("execution closed as unreported")
		}
		return errors.New("execution has no durable observation")
	}
	taskID := ""
	if task {
		taskID = in.Task.PublicID
	}
	// The commit drops the execution's facts.
	after := func(tx *gorm.DB) error {
		return execution(store.G(ctx, tx), in.ID).Updates(map[string]any{"state": core.ExecutionStateCommitted, "operation": in.Operation,
			"digest": in.Digest, "task_id": taskID, "record": gorm.Expr("NULL"), "observation": gorm.Expr("NULL"),
			"updated_at": gorm.Expr("clock_timestamp()")}).Error
	}
	var err error
	if task {
		reg := *in.Task
		reg.Record = in.Record
		err = s.registerTask(ctx, reg, before, after)
	} else {
		err = s.insertExecutionRecord(ctx, in.Record, before, after)
	}
	if errors.Is(err, errExecutionDone) {
		return nil
	}
	return err
}

func (s *Service) CloseExecutionUnreported(ctx context.Context, in core.ExecutionUnreported) error {
	// The stored record is immutable once observed, so it is read before
	// the transaction; the lock below rechecks the state it was read in.
	var x model.PluginExecution
	if err := execution(store.G(ctx, s.db.Gorm), in.ID).Select("state, request_id, plugin_key, account_id, record").Take(&x).Error; err != nil {
		return err
	}
	if err := closableExecution(x); err != nil {
		return ignoreDone(err)
	}
	var rec core.UsageRecord
	if err := json.Unmarshal(x.Record, &rec); err != nil {
		return err
	}
	before := func(tx *gorm.DB) error {
		x, err := lockExecution(ctx, tx, in.ID)
		if err != nil {
			return err
		}
		return closableExecution(x)
	}
	after := func(tx *gorm.DB) error {
		return execution(store.G(ctx, tx), in.ID).Updates(map[string]any{"state": core.ExecutionStateUnreported,
			"updated_at": gorm.Expr("clock_timestamp()")}).Error
	}
	// A request that already has its usage record is closed without another:
	// the gateway submitted the free record itself when it could not confirm
	// the observation (stored, but its acknowledgement lost).
	free := unreportedRecord(&rec, in.Reason)
	_, err := s.insertAtomic(ctx, []*core.UsageRecord{free}, before, func(tx *gorm.DB, _ map[string]int64) error { return after(tx) }, true)
	if err != nil {
		return ignoreDone(err)
	}
	slog.ErrorContext(ctx, "usage: plugin execution closed as unreported, not billed", "request_id", x.RequestID,
		"execution_id", in.ID, "plugin", x.PluginKey, "account_id", x.AccountID, "reason", in.Reason)
	return nil
}

// closableExecution says whether x may be closed as unreported: only an
// observed execution is; closing one already unreported is done.
func closableExecution(x model.PluginExecution) error {
	switch core.ValueOr(x.State, "") {
	case core.ExecutionStateObserved:
		return nil
	case core.ExecutionStateUnreported:
		return errExecutionDone
	case core.ExecutionStateCommitted:
		return core.ErrConflict.WithMessage("execution facts already committed")
	}
	return errors.New("execution has no durable observation")
}

func ignoreDone(err error) error {
	if errors.Is(err, errExecutionDone) {
		return nil
	}
	return err
}

// unreportedRecord is the free usage record of an execution the plugin
// stated no facts for: the host's reading with nothing to bill.
func unreportedRecord(rec *core.UsageRecord, reason string) *core.UsageRecord {
	r := *rec
	r.Success, r.ErrorType, r.ErrorMessage = false, core.UsageErrorUnreported, trunc(reason, 1000)
	r.Tokens, r.Metrics, r.Additional, r.Replacement = core.UsageTokens{}, nil, nil, nil
	r.Billable, r.Price, r.BillingError, r.Reservation, r.ReservationDropped = false, nil, "", nil, ""
	r.UsageExtract = core.UsageExtractUnreported
	return &r
}

// insertExecutionRecord writes the usage record of an execution in one
// transaction between before and after; inserting it settles it or, free,
// releases the request's precharge.
func (s *Service) insertExecutionRecord(ctx context.Context, rec *core.UsageRecord, before func(*gorm.DB) error, after func(*gorm.DB) error) error {
	_, err := s.insertAtomic(ctx, []*core.UsageRecord{rec}, before, func(tx *gorm.DB, ids map[string]int64) error {
		if _, ok := ids[rec.RequestID]; !ok {
			return errors.New("usage identity already recorded")
		}
		return after(tx)
	}, true)
	return err
}

// lockExecution reads the execution row id and keeps it locked to the end
// of the transaction tx.
func lockExecution(ctx context.Context, tx *gorm.DB, id string) (model.PluginExecution, error) {
	var x model.PluginExecution
	if err := store.RequireTx(tx); err != nil {
		return x, err
	}
	err := execution(store.G(ctx, tx), id).Select("state, operation, digest, request_id, plugin_key, account_id").
		Clauses(store.ForUpdate()).Take(&x).Error
	return x, err
}

// executionRecoveryAge is how long an observed execution may wait for its
// facts before the recovery sweep closes it as unreported. The sweep is for
// executions whose gateway is gone; a live one has no bound of its own, so
// this stays long until executions report their liveness.
const executionRecoveryAge = time.Hour

// recoverExecutions closes the observed synchronous executions that neither
// committed nor were closed within executionRecoveryAge as unreported: the
// host's reading stays as evidence and is never billed. A task submission
// is left alone; its receipt keeps the upstream answer.
func (s *Service) recoverExecutions(ctx context.Context) error {
	var ids []string
	if err := store.G(ctx, s.db.Gorm).Model(&model.PluginExecution{}).
		Where("state=? AND NOT task_submit AND updated_at<clock_timestamp()-make_interval(secs=>?)", core.ExecutionStateObserved, executionRecoveryAge.Seconds()).
		Order("updated_at").Limit(100).Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		// A conflict is an execution committed meanwhile: its facts stand.
		err := s.CloseExecutionUnreported(ctx, core.ExecutionUnreported{ID: id, Reason: "no facts within the recovery age of the observation"})
		if err != nil && core.AsError(err).Code != core.ErrConflict.Code {
			s.deferExecutionRecovery(ctx, id, err)
		}
	}
	return s.cleanupExecutionReceipts(ctx)
}

func (s *Service) deferExecutionRecovery(ctx context.Context, id string, cause error) {
	slog.ErrorContext(ctx, "usage: execution recovery deferred", "execution_id", id, "err", cause)
	// Rotate failures behind other observations. A record that cannot be
	// written must not monopolize the oldest LIMIT batch on every sweep.
	_ = execution(store.G(ctx, s.db.Gorm), id).Where("state=?", core.ExecutionStateObserved).
		Update("updated_at", gorm.Expr("clock_timestamp()")).Error
}

// Retention eligibility is seven days, whatever the state: an unreported
// execution's reading is evidence for that long. Bounded batches catch up
// without monopolizing the settlement retry worker or holding a long
// transaction.
func (s *Service) cleanupExecutionReceipts(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, table := range []string{"plugin_executions", "plugin_monitor_receipts"} {
		for range 20 {
			if ctx.Err() != nil {
				return nil
			}
			// Up to 1000 rows older than seven days, the oldest first.
			g := store.G(ctx, s.db.Gorm)
			old := g.Table(table).Select("id").Where("created_at<clock_timestamp()-interval '7 days'").Order("created_at").Limit(1000)
			res := g.Table(table).Where("id IN (?)", old).Delete(nil)
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

// execution is the plugin_executions row id.
func execution(db *gorm.DB, id string) *gorm.DB {
	return db.Model(&model.PluginExecution{}).Where("id = ?", id)
}

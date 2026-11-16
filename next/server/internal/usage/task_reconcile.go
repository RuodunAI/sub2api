package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type taskClaim struct {
	id, token, kind, state     string
	snapshot                   []byte
	paths                      []string
	failureCode, failureReason string
	groupID                    int64
}

var errTaskDeferred = errors.New("original task account is unavailable or busy")

func (s *Service) claimTasks(ctx context.Context, limit int) ([]*settleEntry, error) {
	// The due task observations are leased to a new token (leaving
	// updated_at alone) and read with their legacy entry, if any.
	g := store.G(ctx, s.db.Gorm)
	due := g.Model(&model.AsyncTask{}).Select("public_id").
		Where("observation_status='pending' AND next_check_at<=now() AND (lease_until IS NULL OR lease_until<=now())").
		Order("next_check_at, public_id").Limit(limit).Clauses(store.ForUpdate().SkipLocked())
	var claimed []model.AsyncTask
	if err := g.Model(&claimed).Clauses(clause.Returning{}).Where("public_id IN (?)", due).Updates(map[string]any{
		"claim_token": uuid.NewString(), "lease_until": gorm.Expr("now()+make_interval(secs=>?)", int(reconcileLease/time.Second)),
	}).Error; err != nil || len(claimed) == 0 {
		return nil, err
	}
	// The legacy settlement entry of each task, if any.
	ids := make([]string, len(claimed))
	for i, t := range claimed {
		ids[i] = t.PublicID
	}
	var entries []model.PendingSettlement
	if err := g.Select("id, task_public_id").Where("task_public_id = ANY(?)", store.Strings(ids)).Find(&entries).Error; err != nil {
		return nil, err
	}
	entryOf := make(map[string]int64, len(entries))
	for _, p := range entries {
		entryOf[*p.TaskPublicID] = p.ID
	}
	out := make([]*settleEntry, len(claimed))
	for i, t := range claimed {
		accountID := t.AccountID
		out[i] = &settleEntry{task: &taskClaim{id: t.PublicID, token: t.ClaimToken, kind: t.Kind, state: core.ValueOr(t.State, ""), groupID: t.GroupID},
			pluginKey: t.PluginKey, refID: t.UpstreamRefID, usageLogID: t.UsageLogID, accountID: &accountID,
			attempts: t.Attempts, pollFailures: t.PollFailures, createdAt: t.CreatedAt, deadlineAt: t.DeadlineAt, id: entryOf[t.PublicID]}
	}
	return out, nil
}

func (s *Service) acquireTaskAccount(ctx context.Context, e *settleEntry) (context.Context, *pluginv1.Account, *int64, func(), error) {
	if e.accountID == nil {
		return ctx, nil, nil, nil, errTaskDeferred
	}
	a, err := s.rec.Accounts.Load(ctx, *e.accountID)
	// A disabled account is only kept out of scheduling: its tasks still settle.
	if err != nil || a == nil || a.PluginKey != e.pluginKey {
		return ctx, nil, nil, nil, errTaskDeferred
	}
	cooling, err := s.rec.Accounts.IsCoolingDown(ctx, a.ID)
	if err != nil || cooling {
		return ctx, nil, nil, nil, errTaskDeferred
	}
	release := func() {}
	ok := true
	if s.rec.Slots != nil {
		ctx, release, ok, err = core.AcquireSlot(ctx, s.rec.Slots, "account", a.ID, a.MaxConcurrency, "poll:"+e.pluginKey+":"+e.refID)
	}
	if err != nil || !ok {
		return ctx, nil, nil, nil, errTaskDeferred
	}
	e.account = a.AccountRef
	return ctx, a.PluginAccount(""), a.ProxyID, release, nil
}

func (s *Service) reconcileTask(ctx context.Context, e *settleEntry) bool {
	row, err := s.loadReserved(ctx, e.usageLogID)
	if err != nil {
		s.rec.log.Error("task usage lookup failed", "task", e.task.id, "err", err)
		return false
	}
	cfg := s.reconcileSettings(ctx)
	if !time.Now().Before(e.deadlineAt) {
		keptEstimate := e.task.state == "succeeded" && row.status == StatusReserved
		s.failObservation(ctx, e, row, "task_timeout", "task observation deadline reached")
		return keptEstimate
	}
	res, err := s.askPlugin(ctx, e, row)
	if e.reported {
		return false
	}
	if err != nil {
		s.handlePollError(ctx, e, row, cfg, err)
		return false
	}
	if s.handlePollResult(ctx, e, row, cfg, res) {
		return false
	}
	terminal := res.GetState() != pluginv1.ReconcileResult_PENDING
	if raw := []byte(res.GetTaskSnapshotJson()); len(raw) > 0 {
		// Paths are captured from the same generation as the parser call.
		if _, err = protocol.RewriteTaskID(raw, e.task.paths, e.refID, e.task.id); err == nil {
			e.task.snapshot = raw
		}
	} else if terminal {
		err = errors.New("terminal task observation is missing its snapshot")
	}
	if err != nil {
		s.handlePollError(ctx, e, row, cfg, err)
		return false
	}
	if !terminal {
		s.rescheduleTask(ctx, e, cfg, time.Duration(res.GetNextCheckAfterSec())*time.Second, "", true)
		return false
	}
	switch res.GetState() {
	case pluginv1.ReconcileResult_SETTLED, pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		e.task.state = "succeeded"
	case pluginv1.ReconcileResult_FAILED:
		e.task.state = "failed"
	default:
		s.rescheduleTask(ctx, e, cfg, 0, "unknown task result state", true)
		return false
	}
	if row.status != StatusReserved {
		s.finishTaskOnly(ctx, e, "closed")
		return false
	}
	switch res.GetState() {
	case pluginv1.ReconcileResult_SETTLED:
		s.settleReconciled(ctx, e, row, res)
	case pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		s.settleEstimate(ctx, e, row, res)
	case pluginv1.ReconcileResult_FAILED:
		s.refundFailed(ctx, e, row, res.GetReason())
	}
	return false
}

// lockTaskOutcome is always called before locking usage/pending rows. Its
// update and the billing outcome share one transaction, so stale workers can
// neither publish a terminal snapshot nor touch money after lease takeover.
func (s *Service) lockTaskOutcome(ctx context.Context, tx *gorm.DB, e *settleEntry, observation string) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	if e.task == nil {
		return nil
	}
	t := e.task
	g := store.G(ctx, tx)
	var current string
	err := asyncTask(g, t.id).Select("state").Where("claim_token=? AND observation_status='pending' AND lease_until>clock_timestamp()", t.token).
		Clauses(store.ForUpdate()).Row().Scan(&current)
	if store.IsNoRows(err) {
		return errNotPending
	}
	if err != nil {
		return err
	}
	// Recheck after a row-lock wait: the statement's original visibility test
	// may have run before the lease expired while waiting for the lock.
	var valid bool
	if err := asyncTask(g, t.id).Select("lease_until>clock_timestamp()").Row().Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errNotPending
	}
	// A manual retry can reopen billing after a confirmed estimate. Pending
	// observations must never replace the already confirmed terminal snapshot.
	snapshot := t.snapshot
	paths := jsonOr(t.paths, "[]")
	state := t.state
	if observation == "pending" {
		if current != "pending" || state != "pending" {
			snapshot = nil
		}
		state = current
	}
	// The observation is written and the claim given up. An empty snapshot
	// stays an empty bytea (store.Bytes), a nil one keeps the stored one.
	return asyncTask(g, t.id).Updates(map[string]any{
		"state": state, "observation_status": observation,
		"snapshot":          gorm.Expr("COALESCE(?,snapshot)", store.Bytes(snapshot)),
		"snapshot_id_paths": gorm.Expr("CASE WHEN ?::bytea IS NULL THEN snapshot_id_paths ELSE ?::jsonb END", store.Bytes(snapshot), store.Bytes(paths)),
		"claim_token":       "", "lease_until": gorm.Expr("NULL"),
		"attempts":     gorm.Expr("attempts+CASE WHEN ?='closed' THEN 1 ELSE 0 END", observation),
		"failure_code": gorm.Expr("CASE WHEN ?='closed' THEN ? ELSE failure_code END", observation, t.failureCode),
		"last_error":   gorm.Expr("CASE WHEN ?='closed' THEN ? ELSE last_error END", observation, t.failureReason),
		"poll_failures": gorm.Expr("CASE WHEN ?='closed' AND ? THEN poll_failures+1 WHEN ?='closed' AND ? THEN 0 ELSE poll_failures END",
			observation, e.pollFailed, observation, e.pollObserved),
		"updated_at": gorm.Expr("now()"),
	}).Error
}

// asyncTask is the async_tasks row id.
func asyncTask(db *gorm.DB, id string) *gorm.DB {
	return db.Model(&model.AsyncTask{}).Where("public_id=?", id)
}

// A legacy worker may have claimed this row before lazy import transferred
// polling to async_tasks. Financial callers already hold the usage row here.
func lockLegacyOutcome(ctx context.Context, tx *gorm.DB, e *settleEntry) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	if e.task != nil {
		return nil
	}
	var managed *string
	if err := store.G(ctx, tx).Model(&model.PendingSettlement{}).Select("task_public_id").Where("id = ?", e.id).
		Clauses(store.ForUpdate()).Row().Scan(&managed); err != nil {
		return err
	}
	if managed != nil {
		return errNotPending
	}
	return nil
}

func (s *Service) finishTaskOnly(ctx context.Context, e *settleEntry, observation string) {
	ctx, cancel := bookkeeping(ctx)
	defer cancel()
	err := s.outcomeTx(ctx, e, func(tx *gorm.DB) error { return s.lockTaskOutcome(ctx, tx, e, observation) })
	if err != nil && !errors.Is(err, errNotPending) {
		s.rec.log.Error("task outcome write failed", "task", e.task.id, "err", err)
	}
}

func (s *Service) rescheduleTask(ctx context.Context, e *settleEntry, cfg resolved, want time.Duration, reason string, count bool) {
	ctx, cancel := bookkeeping(ctx)
	defer cancel()
	n := 0
	if count {
		n = 1
	}
	backoffAttempt := e.attempts + n
	if e.pollFailed {
		backoffAttempt = e.pollFailures
	}
	next := time.Now().Add(cfg.clampDelay(want, backoffAttempt))
	if next.After(e.deadlineAt) {
		next = e.deadlineAt
	}
	err := s.outcomeTx(ctx, e, func(tx *gorm.DB) error {
		if err := s.lockTaskOutcome(ctx, tx, e, "pending"); err != nil {
			return err
		}
		g := store.G(ctx, tx)
		err := rescheduleRow(g, "async_tasks", "public_id", e.task.id, e, n, next, reason).Error
		if err == nil && e.id != 0 {
			err = rescheduleRow(g, "pending_settlements", "id", e.id, e, n, next, reason).Error
		}
		return err
	})
	if err != nil && !errors.Is(err, errNotPending) {
		s.rec.log.Error("task reschedule failed", "task", e.task.id, "err", err)
	}
}

// rescheduleRow moves the next check of a task's row in table (async_tasks
// or its pending_settlements entry, keyed by column) and counts the attempt.
// It leaves updated_at alone, as the task's other bookkeeping writes do.
func rescheduleRow(db *gorm.DB, table, key string, id any, e *settleEntry, n int, next time.Time, reason string) *gorm.DB {
	return db.Table(table).Where(key+"=?", id).Updates(map[string]any{
		"attempts": gorm.Expr("attempts+?", n), "next_check_at": next, "last_error": trunc(reason, 2000), "poll_failures": pollFailures(e),
	})
}

func taskPaths(gen core.Generation, e *settleEntry) ([]string, error) {
	for _, b := range gen.Endpoints() {
		if b.Plugin.Key == e.pluginKey && b.Endpoint.TaskQuery() && b.Endpoint.Task.Kind == e.task.kind {
			return append([]string(nil), b.Endpoint.Task.IDPaths...), nil
		}
	}
	return nil, fmt.Errorf("task query contract %s/%s unavailable", e.pluginKey, e.task.kind)
}

package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

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
	rows, err := claimDueTasks(store.G(ctx, s.db.Gorm), limit, uuid.NewString()).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*settleEntry
	for rows.Next() {
		e := &settleEntry{task: &taskClaim{}}
		if err := rows.Scan(&e.task.id, &e.task.token, &e.task.kind, &e.task.state, &e.pluginKey, &e.refID, &e.usageLogID, &e.accountID,
			&e.attempts, &e.pollFailures, &e.createdAt, &e.deadlineAt, &e.id, &e.task.groupID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// claimDueTasks leases up to limit due task observations to token (an
// UPDATE of async_tasks that leaves updated_at alone) and reads them with
// their legacy entry, if any.
func claimDueTasks(db *gorm.DB, limit int, token string) *gorm.DB {
	return db.Raw(`WITH claimed AS (
		UPDATE async_tasks t SET claim_token=?,lease_until=now()+make_interval(secs=>?)
		FROM (SELECT public_id FROM async_tasks WHERE observation_status='pending' AND next_check_at<=now()
			AND (lease_until IS NULL OR lease_until<=now()) ORDER BY next_check_at,public_id FOR UPDATE SKIP LOCKED LIMIT ?) due
		WHERE t.public_id=due.public_id RETURNING t.*)
		SELECT t.public_id,t.claim_token,t.kind,t.state,t.plugin_key,t.upstream_ref_id,t.usage_log_id,t.account_id,
			t.attempts,t.poll_failures,t.created_at,t.deadline_at,COALESCE(p.id,0),t.group_id
		FROM claimed t LEFT JOIN pending_settlements p ON p.task_public_id=t.public_id`, token, int(reconcileLease/time.Second), limit)
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
	if e.task == nil {
		return nil
	}
	t := e.task
	g := store.G(ctx, tx)
	var current string
	err := g.Raw(`SELECT state FROM async_tasks WHERE public_id=? AND claim_token=?
		AND observation_status='pending' AND lease_until>clock_timestamp() FOR UPDATE`, t.id, t.token).Row().Scan(&current)
	if store.IsNoRows(err) {
		return errNotPending
	}
	if err != nil {
		return err
	}
	// Recheck after a row-lock wait: the statement's original visibility test
	// may have run before the lease expired while waiting for the lock.
	var valid bool
	if err := g.Raw(`SELECT lease_until>clock_timestamp() FROM async_tasks WHERE public_id=?`, t.id).Row().Scan(&valid); err != nil {
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
	return releaseTaskClaim(g, e, state, observation, snapshot, paths).Error
}

// releaseTaskClaim writes a task's observation and gives up its claim.
// Arguments are positional, repeated where the statement reuses one.
func releaseTaskClaim(db *gorm.DB, e *settleEntry, state, observation string, snapshot, paths []byte) *gorm.DB {
	t := e.task
	return db.Exec(`UPDATE async_tasks SET state=?,observation_status=?,
		snapshot=COALESCE(?,snapshot),snapshot_id_paths=CASE WHEN ?::bytea IS NULL THEN snapshot_id_paths ELSE ?::jsonb END,
		claim_token='',lease_until=NULL,attempts=attempts+CASE WHEN ?='closed' THEN 1 ELSE 0 END,
		failure_code=CASE WHEN ?='closed' THEN ? ELSE failure_code END,
		last_error=CASE WHEN ?='closed' THEN ? ELSE last_error END,
		poll_failures=CASE WHEN ?='closed' AND ? THEN poll_failures+1 WHEN ?='closed' AND ? THEN 0 ELSE poll_failures END,
		updated_at=now() WHERE public_id=?`,
		state, observation, store.Bytes(snapshot), store.Bytes(snapshot), paths, observation, observation, t.failureCode, observation, t.failureReason,
		observation, e.pollFailed, observation, e.pollObserved, t.id)
}

// A legacy worker may have claimed this row before lazy import transferred
// polling to async_tasks. Financial callers already hold the usage row here.
func lockLegacyOutcome(ctx context.Context, tx *gorm.DB, e *settleEntry) error {
	if e.task != nil {
		return nil
	}
	var managed *string
	if err := store.G(ctx, tx).Model(&model.PendingSettlement{}).Select("task_public_id").Where("id = ?", e.id).
		Clauses(store.ForUpdate).Row().Scan(&managed); err != nil {
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
	return db.Exec(`UPDATE `+table+` SET attempts=attempts+?,next_check_at=?,last_error=?,
		poll_failures=CASE WHEN ? THEN poll_failures+1 WHEN ? THEN 0 ELSE poll_failures END WHERE `+key+`=?`,
		n, next, trunc(reason, 2000), e.pollFailed, e.pollObserved, id)
}

func taskPaths(gen core.Generation, e *settleEntry) ([]string, error) {
	for _, b := range gen.Endpoints() {
		if b.Plugin.Key == e.pluginKey && b.Endpoint.TaskQuery() && b.Endpoint.Task.Kind == e.task.kind {
			return append([]string(nil), b.Endpoint.Task.IDPaths...), nil
		}
	}
	return nil, fmt.Errorf("task query contract %s/%s unavailable", e.pluginKey, e.task.kind)
}

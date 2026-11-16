package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type monitorOutcome struct {
	ctx                context.Context
	id, digest, taskID string
	err                error
	done               bool
}

// outcomeTx adds the result receipt to the existing financial transaction.
// Its advisory lock serializes retries of this claim, before task→usage→pending.
func (s *Service) outcomeTx(ctx context.Context, e *settleEntry, fn func(*gorm.DB) error) error {
	m := e.monitor
	if m == nil {
		return s.db.Tx(ctx, fn)
	}
	ctx = m.ctx
	if err := ctx.Err(); err != nil {
		m.err = err
		return err
	}
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := store.XactLock(ctx, tx, "plugin-monitor:"+m.id); err != nil {
			return err
		}
		var digest string
		err := g.Model(&model.PluginMonitorReceipt{}).Select("digest").Where("id = ?", m.id).Row().Scan(&digest)
		if err == nil {
			if digest != m.digest {
				return core.ErrConflict.WithMessage("monitor result already committed")
			}
			return nil
		}
		if !store.IsNoRows(err) {
			return err
		}
		if err = fn(tx); err != nil {
			return err
		}
		return g.Create(&model.PluginMonitorReceipt{ID: m.id, Digest: m.digest, TaskID: m.taskID}).Error
	})
	m.err = err
	m.done = err == nil
	return err
}

func (s *Service) askMonitor(ctx context.Context, e *settleEntry, row *reservedRowState, call observationCall) (*pluginv1.ReconcileResult, error) {
	monitor, ok := call.client.(core.MonitorPlugin)
	if !ok {
		return nil, errors.New("platform.monitor.v1 has no Monitor implementation")
	}
	claimID := fmt.Sprintf("legacy:%d:%s", e.id, uuid.NewString())
	taskID := ""
	if e.task != nil {
		claimID = e.task.id + ":" + e.task.token
		taskID = e.task.id
	}
	var deferred atomic.Bool
	execute := func(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
		return s.fetchExecutionHTTP(ctx, in, call.proxyID, func(ctx context.Context) error {
			if s.rec.Limiter != nil {
				ok, err := s.rec.Limiter.TryHit(ctx, e.account, "", "")
				if err != nil || !ok {
					deferred.Store(true)
					return errTaskDeferred
				}
			}
			return nil
		})
	}
	report := func(ctx context.Context, in *pluginv1.ReportTaskProgressRequest) (*pluginv1.ExecutionReceipt, error) {
		if deferred.Load() {
			return nil, errTaskDeferred
		}
		if in.Result == nil {
			return nil, errors.New("missing monitor result")
		}
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(in.Result)
		if err != nil {
			return nil, err
		}
		if len(raw) > 512<<10 {
			return nil, errors.New("monitor result too large")
		}
		sum := sha256.Sum256(raw)
		e.monitor = &monitorOutcome{ctx: ctx, id: claimID, digest: hex.EncodeToString(sum[:]), taskID: taskID}
		defer func() { e.monitor = nil }()
		if err = s.applyMonitorResult(ctx, e, row, in.Result); err != nil {
			return nil, err
		}
		if !e.monitor.done {
			return nil, errors.New("monitor result was not committed")
		}
		e.reported = true
		return &pluginv1.ExecutionReceipt{TaskId: taskID}, nil
	}
	err := monitor.Monitor(ctx, &pluginv1.PollRequest{Entry: call.entry, Account: call.acct}, execute, report)
	if deferred.Load() && !e.reported {
		return nil, errTaskDeferred
	}
	return nil, err
}

func (s *Service) applyMonitorResult(ctx context.Context, e *settleEntry, row *reservedRowState, res *pluginv1.ReconcileResult) error {
	cfg := s.reconcileSettings(ctx)
	if s.handlePollResult(ctx, e, row, cfg, res) {
		return e.monitor.err
	}
	state := res.GetState()
	if state < pluginv1.ReconcileResult_PENDING || state > pluginv1.ReconcileResult_SETTLED_ESTIMATE {
		return errors.New("unknown monitor result state")
	}
	if e.task != nil {
		e.task.snapshot = nil
		if raw := []byte(res.TaskSnapshotJson); len(raw) > 0 {
			if _, err := protocol.RewriteTaskID(raw, e.task.paths, e.refID, e.task.id); err != nil {
				return err
			}
			e.task.snapshot = raw
		} else if state != pluginv1.ReconcileResult_PENDING {
			return errors.New("terminal monitor result missing snapshot")
		}
		if state == pluginv1.ReconcileResult_PENDING {
			s.rescheduleTask(ctx, e, cfg, time.Duration(res.NextCheckAfterSec)*time.Second, "", true)
			return e.monitor.err
		}
		if state == pluginv1.ReconcileResult_FAILED {
			e.task.state = "failed"
		} else {
			e.task.state = "succeeded"
		}
		if row.status != StatusReserved {
			s.finishTaskOnly(ctx, e, "closed")
			return e.monitor.err
		}
	} else if state == pluginv1.ReconcileResult_PENDING {
		next := time.Now().Add(cfg.clampDelay(time.Duration(res.NextCheckAfterSec)*time.Second, e.attempts+1))
		if next.After(e.deadlineAt) {
			next = e.deadlineAt
		}
		return s.outcomeTx(ctx, e, func(tx *gorm.DB) error {
			// Match legacy finance lock order and reject a row transferred to the
			// managed task table while this callback was running.
			status, err := lockUsageRow(ctx, tx, e.usageLogID)
			if err != nil {
				return err
			}
			if status != StatusReserved {
				return errNotPending
			}
			if err := lockLegacyOutcome(ctx, tx, e); err != nil {
				return err
			}
			res := settlementByID(store.G(ctx, tx), e.id).Where("state = 'pending'").Updates(map[string]any{
				"attempts": gorm.Expr("attempts+1"), "poll_failures": 0, "next_check_at": next, "last_error": ""})
			if res.Error == nil && res.RowsAffected != 1 {
				return errNotPending
			}
			return res.Error
		})
	}
	switch state {
	case pluginv1.ReconcileResult_SETTLED:
		s.settleReconciled(ctx, e, row, res)
	case pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		s.settleEstimate(ctx, e, row, res)
	case pluginv1.ReconcileResult_FAILED:
		s.refundFailed(ctx, e, row, res.Reason)
	}
	return e.monitor.err
}

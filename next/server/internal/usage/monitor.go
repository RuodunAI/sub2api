package usage

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// askMonitor runs the plugin's Monitor once for e on the account call
// holds. Any error is one failed query; a fetch the account's limiter
// refused is errTaskDeferred, whatever the plugin made of it.
func (s *Service) askMonitor(ctx context.Context, e *settleEntry, call observationCall) (*pluginv1.ReconcileResult, error) {
	var deferred atomic.Bool
	fetch := func(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
		return s.fetchExecutionHTTP(ctx, in, e.account.ID, func(ctx context.Context) error {
			if s.rec.Limiter == nil {
				return nil
			}
			if ok, err := s.rec.Limiter.TryHit(ctx, e.account, "", ""); err != nil || !ok {
				deferred.Store(true)
				return errTaskDeferred
			}
			return nil
		})
	}
	res, err := call.client.Monitor(ctx, &pluginv1.PollRequest{Entry: call.entry, Account: call.acct}, fetch)
	switch {
	case deferred.Load():
		return nil, errTaskDeferred
	case err != nil:
		return nil, err
	case res == nil:
		return nil, errors.New("Monitor returned no result")
	}
	return res, nil
}

// observe asks the plugin about e once and applies the answer. An outcome
// whose transaction fails is logged and left alone: the claim's lease
// lapses and the next round asks again.
func (s *Service) observe(ctx context.Context, e *settleEntry, row *reservedRowState) {
	cfg := s.reconcileSettings(ctx)
	res, err := s.askPlugin(ctx, e, row)
	if err != nil {
		// Query failures have a persisted consecutive-failure budget. Local
		// account deferrals and caller cancellation do not spend that budget.
		s.handlePollError(ctx, e, row, cfg, err)
		return
	}
	if s.handlePollResult(ctx, e, row, cfg, res) {
		return
	}
	if err := taskObservation(e, res); err != nil {
		s.handlePollError(ctx, e, row, cfg, err)
		return
	}
	state := res.GetState()
	if state == pluginv1.ReconcileResult_PENDING {
		// A plugin that cannot tell yet means "ask again" - never "settle
		// for nothing".
		s.reschedule(ctx, e, cfg, time.Duration(res.GetNextCheckAfterSec())*time.Second, "")
		return
	}
	if e.task != nil && row.status != StatusReserved {
		s.finishTaskOnly(ctx, e, "closed")
		return
	}
	switch state {
	case pluginv1.ReconcileResult_SETTLED:
		s.settleReconciled(ctx, e, row, res)
	case pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		s.settleEstimate(ctx, e, row, res)
	case pluginv1.ReconcileResult_FAILED:
		s.refundFailed(ctx, e, row, res.GetReason())
	}
}

// taskObservation takes a task's snapshot and state from a result of one
// of the states handlePollResult passes on. A terminal one must carry the
// snapshot the client will be shown.
func taskObservation(e *settleEntry, res *pluginv1.ReconcileResult) error {
	t := e.task
	if t == nil {
		return nil
	}
	state := res.GetState()
	t.snapshot = nil
	if raw := []byte(res.GetTaskSnapshotJson()); len(raw) > 0 {
		// Paths are captured from the same generation as the Monitor call.
		if _, err := protocol.RewriteTaskID(raw, t.paths, e.refID, t.id); err != nil {
			return err
		}
		t.snapshot = raw
	} else if state != pluginv1.ReconcileResult_PENDING {
		return errors.New("terminal task observation is missing its snapshot")
	}
	switch state {
	case pluginv1.ReconcileResult_FAILED:
		t.state = "failed"
	case pluginv1.ReconcileResult_SETTLED, pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		t.state = "succeeded"
	}
	return nil
}

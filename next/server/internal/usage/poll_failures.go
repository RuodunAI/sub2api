package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// handlePollResult handles host policy outcomes before protocol snapshots or
// financial facts are interpreted. NOT_FOUND and POLL_FAILED need no snapshot.
func (s *Service) handlePollResult(ctx context.Context, e *settleEntry, row *reservedRowState, cfg resolved, res *pluginv1.ReconcileResult) bool {
	if res == nil {
		s.handlePollError(ctx, e, row, cfg, errors.New("plugin returned no poll result"))
		return true
	}
	switch res.GetState() {
	case pluginv1.ReconcileResult_NOT_FOUND:
		s.failObservation(ctx, e, row, "task_not_found", res.GetReason())
		return true
	case pluginv1.ReconcileResult_POLL_FAILED:
		s.recordPollFailure(ctx, e, row, cfg, res.GetReason(), time.Duration(res.NextCheckAfterSec)*time.Second)
		return true
	case pluginv1.ReconcileResult_PENDING, pluginv1.ReconcileResult_SETTLED, pluginv1.ReconcileResult_FAILED, pluginv1.ReconcileResult_SETTLED_ESTIMATE:
		e.pollObserved = true
		return false
	default:
		s.handlePollError(ctx, e, row, cfg, errors.New("unknown poll result state"))
		return true
	}
}

// errEntryBudget is the cause of an observation that used its whole budget.
var errEntryBudget = fmt.Errorf("no answer within the %s entry budget", reconcileEntryBudget)

func (s *Service) handlePollError(ctx context.Context, e *settleEntry, row *reservedRowState, cfg resolved, err error) {
	// Local account pressure or a cancelled worker did not observe the
	// upstream. A plugin or upstream that used the whole entry budget did not
	// answer the query, which is a failed query like any other.
	budgetSpent := errors.Is(context.Cause(ctx), errEntryBudget)
	if errors.Is(err, errTaskDeferred) || ctx.Err() != nil && !budgetSpent {
		s.rescheduleAttempt(ctx, e, cfg, 0, err.Error(), false)
		return
	}
	reason := err.Error()
	if budgetSpent {
		reason = errEntryBudget.Error()
	}
	s.recordPollFailure(ctx, e, row, cfg, reason, 0)
}

func (s *Service) recordPollFailure(ctx context.Context, e *settleEntry, row *reservedRowState, cfg resolved, reason string, delay time.Duration) {
	// The query may have used the whole entry budget; recording it may not.
	ctx, cancel := bookkeeping(ctx)
	defer cancel()
	e.pollFailed, e.pollObserved = true, false
	if reason == "" {
		reason = "task query failed"
	}
	if e.task != nil {
		cfg = s.withEnvironment(ctx, cfg, e.task.groupID, e.accountID)
	}
	if cfg.maxPollFailures > 0 && e.pollFailures+1 >= cfg.maxPollFailures {
		s.failObservation(ctx, e, row, "task_poll_failed", fmt.Sprintf("%d consecutive query failures: %s", e.pollFailures+1, reason))
		return
	}
	s.rescheduleAttempt(ctx, e, cfg, delay, reason, true)
}

// The upstream may have completed even when it cannot be queried. This is a
// local policy failure, recorded distinctly from an upstream FAILED result.
// Existing transactions make closing the task and refunding indivisible.
func (s *Service) failObservation(ctx context.Context, e *settleEntry, row *reservedRowState, code, reason string) {
	if reason == "" {
		reason = code
	}
	if e.task != nil {
		// A manual billing recheck must not revoke an already delivered result.
		if e.task.state == "succeeded" {
			if row.status == StatusReserved {
				s.abandon(ctx, e, row, code+": "+reason)
			} else {
				s.finishTaskOnly(ctx, e, "abandoned")
			}
			return
		}
		e.task.state = "failed"
		e.task.snapshot = nil
		e.task.failureCode, e.task.failureReason = code, reason
		if row.status != StatusReserved {
			s.finishTaskOnly(ctx, e, "closed")
			return
		}
	}
	s.refundFailed(ctx, e, row, code+": "+reason)
}

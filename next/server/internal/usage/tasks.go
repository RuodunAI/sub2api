package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var _ core.AsyncTasks = (*Service)(nil)

// The statements of the task endpoints are on the request path: BeginTask,
// ReceiveTask and registerTask run for every task submission, FindTask for
// every poll of a client.

func (s *Service) BeginTask(ctx context.Context, in core.TaskIntent) (string, error) {
	// The dispatch identity of the submission; the database fills its
	// state, expiry and timestamps.
	id := "s2task_" + uuid.NewString()
	return id, store.G(ctx, s.db.Gorm).Create(&model.TaskSubmissionReceipt{RequestID: in.RequestID, PublicID: id,
		PluginKey: in.PluginKey, Kind: in.Kind, UserID: in.UserID, APIKeyID: in.APIKeyID, GroupID: in.GroupID,
		AccountID: in.AccountID, Model: in.Model, Protocol: in.Protocol}).Error
}

func (s *Service) ReceiveTask(ctx context.Context, requestID string, status int, body []byte, reason string) error {
	if len(body) > protocol.MaxTaskSnapshotBytes {
		body = nil
		reason = "response exceeds task receipt limit"
	}
	state := "received"
	if reason != "" {
		state = "uncertain"
	}
	// For a submission not yet registered. A nil body keeps the stored one;
	// an empty one is stored.
	return receipt(store.G(ctx, s.db.Gorm), requestID).Where("state <> 'registered'").Updates(map[string]any{
		"state": state, "response_status": status, "response_body": gorm.Expr("COALESCE(?,response_body)", store.Bytes(body)),
		"last_error": trunc(reason, 2000),
	}).Error
}

// RegisterTask commits identity, initial observation, usage and optional debit
// together. The gateway cannot return a public ID before this succeeds.
func (s *Service) RegisterTask(ctx context.Context, in core.TaskRegistration) error {
	return s.registerTask(ctx, in, nil, nil, false)
}

func (s *Service) registerTask(ctx context.Context, in core.TaskRegistration, before func(*gorm.DB) error, after func(*gorm.DB) error, strict bool) error {
	r := in.Record
	if r == nil || r.AccountID == nil || in.PublicID == "" || in.UpstreamID == "" {
		return errors.New("invalid task registration")
	}
	if _, err := protocol.RewriteTaskID(in.Snapshot, in.IDPaths, in.UpstreamID, in.PublicID); err != nil {
		return err
	}
	cfg := s.withEnvironment(ctx, s.reconcileSettings(ctx), r.GroupID, r.AccountID)
	now := time.Now()
	deadline := now.Add(cfg.clampDeadline(in.Deadline))
	next := now.Add(cfg.clampDelay(in.NextCheckAfter, 0))
	if next.After(deadline) {
		next = deadline
	}
	if r.Reservation != nil {
		r.Reservation.RefID = in.PublicID
	}
	_, err := s.insertAtomic(ctx, []*core.UsageRecord{r}, before, func(tx *gorm.DB, ids map[string]int64) error {
		uid, ok := ids[r.RequestID]
		if !ok {
			return errors.New("task request ID already registered")
		}
		err := recordTask(ctx, tx, in, uid, next, deadline)
		if err == nil && after != nil {
			err = after(tx)
		}
		return err
	}, strict)
	if err != nil {
		return err
	}
	// A committed task follows the ordinary immediate settlement path when
	// its submission reported final usage without a reservation. Failure
	// queues billing for retry; it cannot turn registration into a failed
	// submission or cause the upstream task to be sent again.
	if !strict && initialStatus(r) == StatusPending {
		p := fromRecord(r)
		if err := s.settle(ctx, p, false); err != nil {
			s.markFailed(ctx, p, err)
		}
	}
	return nil
}

// recordTask writes a submitted task in the transaction of its usage row
// uid: it checks the receipt, inserts the task, links the row's
// settlement entry and closes the receipt.
func recordTask(ctx context.Context, tx *gorm.DB, in core.TaskRegistration, uid int64, next, deadline time.Time) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	r := in.Record
	// The receipt is the trusted dispatch identity; parsing cannot substitute it.
	g := store.G(ctx, tx)
	var valid bool
	if err := receipt(g, r.RequestID).Select(`public_id=? AND plugin_key=? AND kind=? AND user_id=?
		AND api_key_id=? AND group_id=? AND account_id=? AND model=? AND protocol=?`,
		in.PublicID, r.PluginKey, in.Kind, r.UserID, r.APIKeyID, r.GroupID, *r.AccountID, r.Model, r.Protocol).
		Where("state='received'").Clauses(store.ForUpdate()).Row().Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("task receipt identity mismatch")
	}
	err := g.Create(&model.AsyncTask{PublicID: in.PublicID, PluginKey: r.PluginKey, Kind: in.Kind, UpstreamRefID: in.UpstreamID,
		UserID: r.UserID, APIKeyID: r.APIKeyID, GroupID: r.GroupID, AccountID: *r.AccountID, Model: r.Model, Protocol: r.Protocol,
		UsageLogID: uid, Snapshot: in.Snapshot, SnapshotIDPaths: jsonOr(in.IDPaths, "[]"), NextCheckAt: next, DeadlineAt: deadline}).Error
	if err != nil {
		return err
	} // Identity collisions must never overwrite ownership.
	if err = g.Model(&model.PendingSettlement{}).Where("usage_log_id=?", uid).Update("task_public_id", in.PublicID).Error; err != nil {
		return err
	}
	return receipt(g, r.RequestID).Updates(map[string]any{"state": "registered", "response_body": gorm.Expr("NULL"),
		"last_error": "", "expires_at": gorm.Expr("LEAST(expires_at,?)", deadline)}).Error
}

// receipt is the task_submission_receipts row of a request.
func receipt(db *gorm.DB, requestID string) *gorm.DB {
	return db.Model(&model.TaskSubmissionReceipt{}).Where("request_id=?", requestID)
}

func (s *Service) FindTask(ctx context.Context, q core.TaskQuery) (*core.TaskSnapshot, error) {
	out, err := readTask(ctx, s.db.Gorm, q)
	if store.IsNoRows(err) && !strings.HasPrefix(q.ID, "s2task_") {
		if err = s.importLegacyTask(ctx, q); err == nil {
			out, err = readTask(ctx, s.db.Gorm, q)
		}
	}
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound.WithMessage("task not found")
	}
	return out, err
}

// readTask reads the task q names: by its public ID, or by the upstream ID
// of an imported legacy task. None is sql.ErrNoRows, two matches are no
// match.
func readTask(ctx context.Context, db *gorm.DB, q core.TaskQuery) (*core.TaskSnapshot, error) {
	rows, err := store.G(ctx, db).Model(&model.AsyncTask{}).Select(`public_id,upstream_ref_id,model,state,
		CASE WHEN state='pending' AND deadline_at<=clock_timestamp() THEN 'abandoned' ELSE observation_status END,
		snapshot,snapshot_id_paths,failure_code,last_error`).
		Where(`plugin_key=? AND kind=? AND user_id=? AND group_id=?
		AND (public_id=? OR (legacy_imported AND upstream_ref_id=? AND NOT starts_with(?,'s2task_')))`,
			q.PluginKey, q.Kind, q.UserID, q.GroupID, q.ID, q.ID, q.ID).
		Order("public_id").Limit(2).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		return nil, sql.ErrNoRows
	}
	out := &core.TaskSnapshot{}
	var paths []byte
	err = rows.Scan(&out.PublicID, &out.UpstreamID, &out.Model, &out.State, &out.ObservationStatus, &out.Body, &paths, &out.FailureCode, &out.FailureReason)
	if rows.Next() {
		return nil, core.ErrNotFound.WithMessage("task not found")
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if err == nil {
		err = json.Unmarshal(paths, &out.IDPaths)
	}
	return out, err
}

// Only core billing history establishes legacy ownership. Plugin tables may
// contain colliding upstream IDs and are deliberately never consulted.
func (s *Service) importLegacyTask(ctx context.Context, q core.TaskQuery) error {
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
		// Lock the usage parent before its settlement, matching the legacy
		// financial path. An imported row cannot be settled by an old claim.
		g := store.G(ctx, tx)
		var ownerUsage int64
		if err := legacyTaskEntries(g, q, "u.id", "u").Row().Scan(&ownerUsage); err != nil {
			return err
		}
		rows, err := legacyTaskEntries(g, q, legacyEntryColumns, "p").Rows()
		if err != nil {
			return err
		}
		var e legacyEntry
		count := 0
		for rows.Next() {
			count++
			err = rows.Scan(&e.id, &e.usageLogID, &e.accountID, &e.model, &e.apiKeyID, &e.next, &e.deadline, &e.created, &e.taskPublicID)
			if err != nil {
				break
			}
		}
		rows.Close()
		if err != nil {
			return err
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if count != 1 || e.accountID == nil {
			return core.ErrNotFound.WithMessage("task not found")
		}
		if e.taskPublicID != nil {
			return nil
		}
		id := "s2task_" + uuid.NewString()
		// Already settled legacy rows still need an observation, without another debit.
		if err = g.Create(&model.AsyncTask{PublicID: id, PluginKey: q.PluginKey, Kind: q.Kind, UpstreamRefID: q.ID, UserID: q.UserID,
			APIKeyID: e.apiKeyID, GroupID: q.GroupID, AccountID: *e.accountID, Model: e.model, Protocol: q.SubmitProtocol,
			UsageLogID: e.usageLogID, SnapshotIDPaths: jsonOr(q.IDPaths, "[]"), LegacyImported: true, NextCheckAt: e.next,
			DeadlineAt: e.deadline, CreatedAt: e.created}).Error; err != nil {
			return err
		}
		// Importing into the managed scheduler must not reset query failures;
		// updated_at keeps the insert's value.
		if err = g.Model(&model.AsyncTask{}).Clauses(store.UpdateFrom("pending_settlements p")).
			Where("async_tasks.public_id=? AND p.id=?", id, e.id).
			Updates(map[string]any{"poll_failures": gorm.Expr("p.poll_failures"), "attempts": gorm.Expr("p.attempts")}).Error; err != nil {
			return err
		}
		return g.Model(&model.PendingSettlement{}).Where("id = ?", e.id).Update("task_public_id", id).Error
	})
}

// legacyEntry is the settlement entry of a legacy task, with what importing
// the task takes from its usage row.
type legacyEntry struct {
	id, usageLogID, apiKeyID int64
	accountID                *int64
	model                    string
	next, deadline, created  time.Time
	taskPublicID             *string
}

// legacyEntryColumns are the columns of a legacyEntry, in its scan order.
const legacyEntryColumns = `p.id,p.usage_log_id,p.account_id,u.model,u.api_key_id,p.next_check_at,p.deadline_at,
	p.created_at,p.task_public_id`

// legacyTaskEntries selects cols of the legacy settlement entries (p) of
// the task q names, joined with their usage rows (u), locking the rows of
// lockOf.
func legacyTaskEntries(db *gorm.DB, q core.TaskQuery, cols, lockOf string) *gorm.DB {
	return db.Table("pending_settlements p").Select(cols).Joins("JOIN usage_logs u ON u.id=p.usage_log_id").
		Where("p.plugin_key=? AND p.ref_id=? AND u.user_id=? AND u.group_id=? AND u.protocol=?",
			q.PluginKey, q.ID, q.UserID, q.GroupID, q.SubmitProtocol).
		Clauses(store.ForUpdate().Of(lockOf))
}

func (s *Service) cleanTaskReceipts(ctx context.Context) {
	g := store.G(ctx, s.db.Gorm)
	expired := g.Model(&model.TaskSubmissionReceipt{}).Select("request_id").Where("expires_at <= now()").Order("expires_at").Limit(200)
	err := g.Where("request_id IN (?)", expired).Delete(&model.TaskSubmissionReceipt{}).Error
	if err != nil {
		s.rec.log.Warn("task receipt cleanup failed", "err", err)
	}
}

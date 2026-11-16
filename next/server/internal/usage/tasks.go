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
// every poll of a client. They are Raw/Exec statements.

func (s *Service) BeginTask(ctx context.Context, in core.TaskIntent) (string, error) {
	// The dispatch identity of the submission; the database fills its
	// state, expiry and timestamps.
	id := "s2task_" + uuid.NewString()
	return id, store.G(ctx, s.db.Gorm).Exec(`INSERT INTO task_submission_receipts
		(request_id, public_id, plugin_key, kind, user_id, api_key_id, group_id, account_id, model, protocol)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, in.RequestID, id, in.PluginKey, in.Kind,
		in.UserID, in.APIKeyID, in.GroupID, in.AccountID, in.Model, in.Protocol).Error
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
	return store.G(ctx, s.db.Gorm).Exec(`UPDATE task_submission_receipts SET state=?, response_status=?,
		response_body=COALESCE(?,response_body), last_error=? WHERE request_id=? AND state <> 'registered'`,
		state, status, store.Bytes(body), trunc(reason, 2000), requestID).Error
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
	r := in.Record
	// The receipt is the trusted dispatch identity; parsing cannot substitute it.
	g := store.G(ctx, tx)
	var valid bool
	if err := g.Raw(`SELECT public_id=? AND plugin_key=? AND kind=? AND user_id=?
		AND api_key_id=? AND group_id=? AND account_id=? AND model=? AND protocol=?
		FROM task_submission_receipts WHERE request_id=? AND state='received' FOR UPDATE`,
		in.PublicID, r.PluginKey, in.Kind, r.UserID, r.APIKeyID, r.GroupID, *r.AccountID, r.Model, r.Protocol, r.RequestID).Row().Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("task receipt identity mismatch")
	}
	err := g.Exec(`INSERT INTO async_tasks(public_id,plugin_key,kind,upstream_ref_id,user_id,api_key_id,
		group_id,account_id,model,protocol,usage_log_id,snapshot,snapshot_id_paths,next_check_at,deadline_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.PublicID, r.PluginKey, in.Kind, in.UpstreamID,
		r.UserID, r.APIKeyID, r.GroupID, *r.AccountID, r.Model, r.Protocol, uid, store.Bytes(in.Snapshot),
		store.Bytes(jsonOr(in.IDPaths, "[]")), next, deadline).Error
	if err != nil {
		return err
	} // Identity collisions must never overwrite ownership.
	if err = g.Exec(`UPDATE pending_settlements SET task_public_id=? WHERE usage_log_id=?`, in.PublicID, uid).Error; err != nil {
		return err
	}
	return g.Exec(`UPDATE task_submission_receipts SET state='registered',response_body=NULL,last_error='',
		expires_at=LEAST(expires_at,?) WHERE request_id=?`, deadline, r.RequestID).Error
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
	rows, err := store.G(ctx, db).Raw(`SELECT public_id,upstream_ref_id,model,state,
		CASE WHEN state='pending' AND deadline_at<=clock_timestamp() THEN 'abandoned' ELSE observation_status END,
		snapshot,snapshot_id_paths,failure_code,last_error
		FROM async_tasks WHERE plugin_key=? AND kind=? AND user_id=? AND group_id=?
		AND (public_id=? OR (legacy_imported AND upstream_ref_id=? AND NOT starts_with(?,'s2task_')))
		ORDER BY public_id LIMIT 2`, q.PluginKey, q.Kind, q.UserID, q.GroupID, q.ID, q.ID, q.ID).Rows()
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
		if err = g.Exec(`INSERT INTO async_tasks(public_id,plugin_key,kind,upstream_ref_id,user_id,api_key_id,group_id,
		account_id,model,protocol,usage_log_id,snapshot_id_paths,legacy_imported,next_check_at,deadline_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,true,?,?,?)`, id, q.PluginKey, q.Kind, q.ID, q.UserID, e.apiKeyID, q.GroupID,
			*e.accountID, e.model, q.SubmitProtocol, e.usageLogID, jsonOr(q.IDPaths, "[]"), e.next, e.deadline, e.created).Error; err != nil {
			return err
		}
		// Importing into the managed scheduler must not reset query failures;
		// updated_at keeps the insert's value.
		if err = g.Exec(`UPDATE async_tasks SET poll_failures=p.poll_failures,attempts=p.attempts
		FROM pending_settlements p WHERE async_tasks.public_id=? AND p.id=?`, id, e.id).Error; err != nil {
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
	return db.Raw(`SELECT `+cols+` FROM pending_settlements p JOIN usage_logs u ON u.id=p.usage_log_id
		WHERE p.plugin_key=? AND p.ref_id=? AND u.user_id=? AND u.group_id=? AND u.protocol=? FOR UPDATE OF `+lockOf,
		q.PluginKey, q.ID, q.UserID, q.GroupID, q.SubmitProtocol)
}

func (s *Service) cleanTaskReceipts(ctx context.Context) {
	err := store.G(ctx, s.db.Gorm).Exec(`DELETE FROM task_submission_receipts WHERE request_id IN
		(SELECT request_id FROM task_submission_receipts WHERE expires_at <= now() ORDER BY expires_at LIMIT 200)`).Error
	if err != nil {
		s.rec.log.Warn("task receipt cleanup failed", "err", err)
	}
}

package helperhistory

import (
	"context"
	"encoding/json"
	"time"

	wire "github.com/Sub2API-Devs/sup2api/next/plugins/ccgateway/companions/contracts/helperhistory"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

const maxUsageBytes = 2 << 20

func usageAAD(requestID, attemptID, digest string, o core.ResourceOwner) []byte {
	b, _ := json.Marshal([]any{"helper-usage-v1", requestID, attemptID, digest, o})
	return b
}
func freezeUsage(a attempt, r *core.UsageRecord) ([]byte, string, error) {
	if r == nil || r.RequestID != a.requestID || r.UserID != a.owner.UserID || r.GroupID != a.owner.GroupID || r.AccountID == nil || *r.AccountID != a.Binding.AccountID {
		return nil, "", core.ErrInvalidArgument.WithMessage("helper usage identity mismatch")
	}
	raw, digest, e := core.HelperHistoryUsageBytes(r)
	if e != nil || len(raw) > maxUsageBytes {
		return nil, "", core.ErrInvalidArgument.WithMessage("helper usage envelope invalid")
	}
	return raw, digest, nil
}
func (s *Service) saveUsage(ctx context.Context, q store.Querier, a attempt, r *core.UsageRecord) error {
	raw, digest, e := freezeUsage(a, r)
	if e != nil {
		return e
	}
	if a.usageDigest != "" {
		if a.usageDigest != digest {
			return core.ErrConflict.WithMessage("helper usage changed after freeze")
		}
		return nil
	}
	db := store.G(ctx, q)
	var old model.ProviderHelperUsageOutbox
	e = outboxDigest(db, a.requestID).Take(&old).Error
	if e == nil {
		if old.Digest != digest {
			return core.ErrConflict.WithMessage("helper usage changed after freeze")
		}
		return nil
	}
	if !store.IsNoRows(e) {
		return e
	}
	encrypted, e := s.cipher.Encrypt(raw, usageAAD(a.requestID, a.ID, digest, a.owner))
	if e != nil {
		return e
	}
	if e = db.Model(&model.ProviderHelperAttempt{}).Where("id = ?", a.ID).Update("usage_digest", digest).Error; e != nil {
		return e
	}
	return db.Create(&model.ProviderHelperUsageOutbox{RequestID: a.requestID, AttemptID: a.ID, UserID: a.owner.UserID, GroupID: a.owner.GroupID,
		Digest: digest, Payload: encrypted, CreatedAt: s.now()}).Error
}

// outboxDigest selects the digest of the outbox entry of requestID, the one
// column its callers compare: the row also holds the encrypted usage, up to
// maxUsageBytes, which they have no use for.
func outboxDigest(db *gorm.DB, requestID string) *gorm.DB {
	return db.Model(&model.ProviderHelperUsageOutbox{}).Select("digest").Where("request_id = ?", requestID)
}
func (s *Service) PersistUncertainUsage(ctx context.Context, o core.ResourceOwner, id string, r *core.UsageRecord) error {
	return s.transaction(ctx, o, func(tx pgx.Tx) error {
		a, e := readAttempt(ctx, tx, o, id)
		if e != nil {
			return e
		}
		if a.State == "committed" {
			return s.saveUsage(ctx, tx, a, r)
		}
		if a.State != "dispatched" && a.State != "uncertain" {
			return core.ErrConflict
		}
		if e = s.saveUsage(ctx, tx, a, r); e != nil {
			return e
		}
		return setAttemptState(store.G(ctx, tx), id, "uncertain").Error
	})
}

// pendingOutbox reads into dst up to limit entries due at now, the longest
// waiting first: the columns delivered, as the raw SQL read them.
func pendingOutbox(db *gorm.DB, now time.Time, limit int, dst *[]model.ProviderHelperUsageOutbox) *gorm.DB {
	return db.Select("request_id, attempt_id, user_id, group_id, digest, payload").Where("next_attempt_at <= ?", now).
		Order("next_attempt_at, created_at, request_id").Limit(limit).Find(dst)
}

// PendingUsage does not consume or lease entries. Consumers must persist by the
// existing usage request-ID unique key before acknowledgement; repeats are safe.
func (s *Service) PendingUsage(ctx context.Context, limit int) ([]core.HelperHistoryUsage, error) {
	if limit < 1 || limit > 100 || s.db == nil || s.cipher == nil {
		return nil, core.ErrInvalidArgument
	}
	var rows []model.ProviderHelperUsageOutbox
	if e := pendingOutbox(store.G(ctx, s.db.Pool), s.now(), limit, &rows).Error; e != nil {
		return nil, e
	}
	out := []core.HelperHistoryUsage{}
	var invalid []struct{ id, digest, code string }
	for _, row := range rows {
		v := core.HelperHistoryUsage{RequestID: row.RequestID, AttemptID: row.AttemptID, Digest: row.Digest}
		owner := core.ResourceOwner{UserID: row.UserID, GroupID: row.GroupID}
		raw, e := s.cipher.Decrypt(row.Payload, usageAAD(v.RequestID, v.AttemptID, v.Digest, owner))
		if e != nil || len(raw) > maxUsageBytes || wire.Digest(raw) != v.Digest {
			invalid = append(invalid, struct{ id, digest, code string }{v.RequestID, v.Digest, "integrity"})
			continue
		}
		record, _, decodeErr := core.DecodeHelperHistoryUsage(raw)
		if decodeErr != nil || record.RequestID != v.RequestID || record.UserID != owner.UserID || record.GroupID != owner.GroupID {
			invalid = append(invalid, struct{ id, digest, code string }{v.RequestID, v.Digest, "envelope"})
			continue
		}
		v.Record = record
		v.FrozenEnvelope = append([]byte(nil), raw...)
		out = append(out, v)
	}
	for _, item := range invalid {
		if err := s.deferUsage(ctx, item.id, item.digest, item.code); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeferUsage retains the immutable evidence and frees the next batch to make
// progress. No error text, payload or credentials are persisted as diagnostics.
func (s *Service) DeferUsage(ctx context.Context, requestID, digest string) error {
	if !bounded(requestID) || !wire.ValidDigest(digest) {
		return core.ErrInvalidArgument
	}
	return s.deferUsage(ctx, requestID, digest, "delivery")
}

func (s *Service) deferUsage(ctx context.Context, requestID, digest, code string) error {
	// Internal callers use the exact stored identity, even when a corrupted
	// digest cannot pass the public delivery contract.
	if s.db == nil {
		return core.ErrInvalidArgument
	}
	return deferOutbox(store.G(ctx, s.db.Pool), requestID, digest, code, s.now().Add(time.Minute)).Error
}

// deferOutbox holds the outbox entry back until at least until and counts
// the retry, saturating short of the int4 range.
func deferOutbox(db *gorm.DB, requestID, digest, code string, until time.Time) *gorm.DB {
	return db.Model(&model.ProviderHelperUsageOutbox{}).Where("request_id = ? AND digest = ?", requestID, digest).Updates(map[string]any{
		"next_attempt_at": gorm.Expr("GREATEST(next_attempt_at, ?)", until),
		"retry_count":     gorm.Expr("LEAST(retry_count, 2147483646) + 1"),
		"failure_code":    code,
	})
}

func (s *Service) AckUsage(ctx context.Context, requestID, digest string) error {
	if !bounded(requestID) || !wire.ValidDigest(digest) {
		return core.ErrInvalidArgument
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		db := store.G(ctx, tx)
		var old model.ProviderHelperUsageOutbox
		e := outboxDigest(db, requestID).Clauses(store.ForUpdate).Take(&old).Error
		if store.IsNoRows(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if old.Digest != digest {
			return core.ErrConflict
		}
		return db.Where("request_id = ?", requestID).Delete(&model.ProviderHelperUsageOutbox{}).Error
	})
}

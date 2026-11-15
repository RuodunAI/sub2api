// Package helperhistory persists private, immutable helper-history chains.
package helperhistory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	wire "github.com/Sub2API-Devs/sup2api/next/plugins/ccgateway/companions/contracts/helperhistory"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/secret"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

type Options struct {
	MaxRecords                    int
	MaxBytes                      int64
	Retention, TombstoneRetention time.Duration
}
type Service struct {
	db      *store.DB
	cipher  *secret.Cipher
	options Options
	now     func() time.Time
}

var _ core.HelperHistory = (*Service)(nil)

func New(db *store.DB, cipher *secret.Cipher, options Options) *Service {
	if options.MaxRecords <= 0 {
		options.MaxRecords = 4096
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 256 << 20
	}
	if options.Retention <= 0 {
		options.Retention = 24 * time.Hour
	}
	if options.TombstoneRetention <= 0 {
		options.TombstoneRetention = 24 * time.Hour
	}
	return &Service{db: db, cipher: cipher, options: options, now: time.Now}
}
func identifier() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}
func validOwner(o core.ResourceOwner) bool { return o.UserID > 0 && o.GroupID > 0 }
func bounded(s string) bool                { return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n") }
func validBinding(b core.ResourceBinding) bool {
	return b.AccountID > 0 && bounded(b.PrincipalID) && bounded(b.Generation)
}
func lock(ctx context.Context, q store.Querier, o core.ResourceOwner) error {
	return store.G(ctx, q).Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, fmt.Sprintf("helper-history:%d:%d", o.UserID, o.GroupID)).Error
}
func (s *Service) transaction(ctx context.Context, o core.ResourceOwner, fn func(pgx.Tx) error) error {
	if !validOwner(o) || s.db == nil || s.cipher == nil {
		return core.ErrInvalidArgument
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if e := lock(ctx, tx, o); e != nil {
			return e
		}
		return fn(tx)
	})
}
func aad(o core.ResourceOwner, r core.HelperHistoryRecord, payloadDigest string) []byte {
	b, _ := json.Marshal([]any{"helper-history-v1", o, r.Receipt, r.ParentReceipt, r.PublicPrefixDigest, r.ChainDigest, r.Namespace, r.Binding, payloadDigest})
	return b
}
func chainDigest(parent string, b core.ResourceBinding, namespace, prefix, payloadDigest string) string {
	raw, _ := json.Marshal([]any{wire.Version, parent, b, namespace, prefix, payloadDigest})
	return wire.Digest(raw)
}
func (s *Service) clean(ctx context.Context, q store.Querier, o core.ResourceOwner, now time.Time) error {
	// Identity tombstones are never automatically evicted: a later identical
	// public prefix must not resurrect an old client with a different hidden chain.
	db := store.G(ctx, q)
	if e := db.Model(&model.ProviderHelperRecord{}).Scopes(owned(o)).Where("expires_at <= ? AND payload IS NOT NULL", now).
		Update("payload", nil).Error; e != nil {
		return e
	}
	return db.Scopes(owned(o)).Where("expires_at <= ?", now.Add(-s.options.TombstoneRetention)).Delete(&model.ProviderHelperAttempt{}).Error
}

// holdings is what an owner holds and has reserved.
type holdings struct{ Count, Size int64 }

// holdingsQuery reads into dst the holdings of owner o: rows and bytes. Every
// live reservation can become both a record and an outbox entry. Existing
// outbox rows are counted once; acknowledged frozen usage needs no future
// outbox reservation. Abort retains only bounded identity metadata. The
// parameters are named, as each is used several times.
func holdingsQuery(db *gorm.DB, o core.ResourceOwner, now time.Time, dst *holdings) *gorm.DB {
	return db.Raw(`WITH owned_attempts AS (
 SELECT a.* FROM provider_helper_attempts a WHERE user_id=@user AND group_id=@group AND state!='committed'
 ) SELECT
 (SELECT count(*) FROM provider_helper_records WHERE user_id=@user AND group_id=@group)+
 (SELECT count(*) FROM provider_helper_usage_outbox WHERE user_id=@user AND group_id=@group)+
 (SELECT coalesce(sum(CASE WHEN state='aborted' THEN 1 WHEN usage_digest!='' THEN 1 ELSE 2 END),0) FROM owned_attempts) AS count,
 (SELECT coalesce(sum(payload_bytes),0) FROM provider_helper_records WHERE user_id=@user AND group_id=@group AND expires_at>@now)+
 (SELECT coalesce(sum(octet_length(payload)),0) FROM provider_helper_usage_outbox WHERE user_id=@user AND group_id=@group)+
 (SELECT coalesce(sum(CASE WHEN state='aborted' THEN 0 ELSE
 CASE WHEN expires_at>@now THEN reserved_bytes+@record_overhead ELSE 0 END+
 CASE WHEN usage_digest='' THEN @usage_reserve ELSE 0 END END),0) FROM owned_attempts) AS size`,
		map[string]any{"user": o.UserID, "group": o.GroupID, "now": now, "record_overhead": 64, "usage_reserve": maxUsageBytes + 64}).
		Scan(dst)
}

func (s *Service) quota(ctx context.Context, q store.Querier, o core.ResourceOwner, now time.Time, bytes int64) error {
	var h holdings
	if e := holdingsQuery(store.G(ctx, q), o, now, &h).Error; e != nil {
		return e
	}
	if h.Count+2 > int64(s.options.MaxRecords) || bytes+64+maxUsageBytes+64 > s.options.MaxBytes-h.Size {
		return core.ErrRateLimited.WithMessage("helper history capacity exhausted")
	}
	return nil
}

// ExpirePayloads clears expired content without deleting the identity tombstone.
// The host maintenance loop must call this even when no new requests arrive.
func (s *Service) ExpirePayloads(ctx context.Context) error {
	if s.db == nil {
		return core.ErrInvalidArgument
	}
	return store.G(ctx, s.db.Pool).Model(&model.ProviderHelperRecord{}).Where("expires_at <= ? AND payload IS NOT NULL", s.now()).
		Update("payload", nil).Error
}

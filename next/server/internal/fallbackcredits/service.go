// Package fallbackcredits binds opaque provider credits to their owner and
// issuing account. Redemption is not marked consumed: provider credits are
// stateless and the provider may permit explicit retries within their lifetime.
package fallbackcredits

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/plugins/ccgateway/companions/contracts/credits"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

type Service struct {
	db          *store.DB
	maxRetained int
}

var _ core.FallbackCredits = (*Service)(nil)

func New(db *store.DB, maxRetained int) *Service {
	if maxRetained <= 0 {
		maxRetained = 10000
	}
	return &Service{db: db, maxRetained: maxRetained}
}
func bounded(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n")
}
func hash(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && strings.ToLower(value) == value
}
func validOwner(owner core.ResourceOwner) bool { return owner.UserID > 0 && owner.GroupID > 0 }

func normalize(in core.FallbackCredit, now time.Time) (core.FallbackCredit, error) {
	if !validOwner(in.Owner) || !hash(in.TokenHash) || !bounded(in.PluginKey) || !bounded(in.SourceModel) || in.Binding.AccountID <= 0 || !bounded(in.Binding.PrincipalID) || !bounded(in.Binding.Generation) || len(in.PromptDigests) < 1 || len(in.PromptDigests) > 3 {
		return in, core.ErrInvalidArgument
	}
	in.PromptDigests = append([]string(nil), in.PromptDigests...)
	slices.Sort(in.PromptDigests)
	for i, value := range in.PromptDigests {
		if !hash(value) || i > 0 && value == in.PromptDigests[i-1] {
			return in, core.ErrInvalidArgument
		}
	}
	in.ObservedAt = in.ObservedAt.UTC().Truncate(time.Microsecond)
	in.ExpiresAt = in.ExpiresAt.UTC().Truncate(time.Microsecond)
	if in.ObservedAt.IsZero() || in.ObservedAt.After(now) || !in.ExpiresAt.After(now) || !in.ExpiresAt.After(in.ObservedAt) || in.ExpiresAt.Sub(in.ObservedAt) > credits.Lifetime {
		return in, core.ErrInvalidArgument.WithMessage("invalid fallback credit observation lifetime")
	}
	return in, nil
}

func read(ctx context.Context, q store.Querier, tokenHash string) (out core.FallbackCredit, err error) {
	var r model.ProviderFallbackCredit
	if err = store.G(ctx, q).Take(&r, "token_hash = ?", tokenHash).Error; err != nil {
		return out, err
	}
	out = core.FallbackCredit{
		TokenHash:   r.TokenHash,
		Owner:       core.ResourceOwner{UserID: r.UserID, GroupID: r.GroupID},
		Binding:     core.ResourceBinding{AccountID: r.AccountID, PrincipalID: r.PrincipalID, Generation: r.Generation},
		PluginKey:   r.PluginKey,
		SourceModel: r.SourceModel,
		ObservedAt:  r.ObservedAt,
		ExpiresAt:   r.ExpiresAt,
	}
	err = json.Unmarshal(r.PromptDigests, &out.PromptDigests)
	return out, err
}

// lockKey takes the transaction-scoped advisory lock of key.
func lockKey(ctx context.Context, tx pgx.Tx, key string) error {
	return store.G(ctx, tx).Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, key).Error
}

func (s *Service) Record(ctx context.Context, input core.FallbackCredit) error {
	in, err := normalize(input, time.Now())
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		owner, _ := json.Marshal(in.Owner)
		if err := lockKey(ctx, tx, "fallback-credit-owner:"+string(owner)); err != nil {
			return err
		}
		if err := lockKey(ctx, tx, "fallback-credit-token:"+in.TokenHash); err != nil {
			return err
		}
		if !in.ExpiresAt.After(time.Now()) {
			return core.ErrInvalidArgument.WithMessage("fallback credit expired before registration")
		}
		old, err := read(ctx, tx, in.TokenHash)
		if err == nil {
			if old.Owner != in.Owner || old.Binding != in.Binding || old.PluginKey != in.PluginKey || old.SourceModel != in.SourceModel || !slices.Equal(old.PromptDigests, in.PromptDigests) {
				return core.ErrConflict.WithMessage("fallback credit registration conflicts with its issuing request")
			}
			// Seeing the same token again never extends its original deadline.
			return nil
		}
		if !store.IsNoRows(err) {
			return err
		}
		// Keep expired hashes briefly as anti-revival tombstones, but bound all
		// retained records rather than only active five-minute entries.
		g := store.G(ctx, tx)
		if err = ownerCredits(g, in.Owner).Where("expires_at < now()-interval '24 hours'").
			Delete(&model.ProviderFallbackCredit{}).Error; err != nil {
			return err
		}
		var retained int64
		if err = ownerCredits(g, in.Owner).Count(&retained).Error; err != nil {
			return err
		}
		if retained >= int64(s.maxRetained) {
			return core.ErrRateLimited.WithMessage("fallback credit tracking capacity exceeded")
		}
		return g.Create(newRow(in)).Error
	})
}

// ownerCredits narrows provider_fallback_credits to owner's records.
func ownerCredits(db *gorm.DB, owner core.ResourceOwner) *gorm.DB {
	return db.Model(&model.ProviderFallbackCredit{}).Where("user_id = ? AND group_id = ?", owner.UserID, owner.GroupID)
}

// newRow is the stored form of a normalized credit.
func newRow(in core.FallbackCredit) *model.ProviderFallbackCredit {
	digests, _ := json.Marshal(in.PromptDigests)
	return &model.ProviderFallbackCredit{
		TokenHash: in.TokenHash, UserID: in.Owner.UserID, GroupID: in.Owner.GroupID,
		AccountID: in.Binding.AccountID, PrincipalID: in.Binding.PrincipalID, Generation: in.Binding.Generation,
		PluginKey: in.PluginKey, SourceModel: in.SourceModel, PromptDigests: digests,
		ObservedAt: in.ObservedAt, ExpiresAt: in.ExpiresAt,
	}
}

func (s *Service) Resolve(ctx context.Context, in core.FallbackCreditLookup) (core.FallbackCredit, error) {
	if !validOwner(in.Owner) || !hash(in.TokenHash) || !hash(in.PromptDigest) {
		return core.FallbackCredit{}, core.ErrInvalidArgument
	}
	record, err := s.LookupOwned(ctx, in.Owner, in.TokenHash)
	if err != nil {
		return core.FallbackCredit{}, err
	}
	if !record.ExpiresAt.After(time.Now()) {
		return core.FallbackCredit{}, core.ErrNotFound
	}
	if !slices.Contains(record.PromptDigests, in.PromptDigest) {
		return core.FallbackCredit{}, core.ErrInvalidArgument.WithMessage("fallback credit retry does not match the issuing prompt and beta headers")
	}
	return record, nil
}

// LookupOwned separates custody from redemption eligibility, including retained expired records.
func (s *Service) LookupOwned(ctx context.Context, owner core.ResourceOwner, tokenHash string) (core.FallbackCredit, error) {
	if !validOwner(owner) || !hash(tokenHash) {
		return core.FallbackCredit{}, core.ErrInvalidArgument
	}
	record, err := read(ctx, s.db.Pool, tokenHash)
	if store.IsNoRows(err) || err == nil && (record.Owner != owner || record.ExpiresAt.Before(time.Now().Add(-24*time.Hour))) {
		return core.FallbackCredit{}, core.ErrNotFound
	}
	if err != nil {
		return core.FallbackCredit{}, err
	}
	return record, nil
}

package ccgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

const reauthLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('ccg-reauth:' || ?::bigint::text, 0))`

// ReauthDraft keeps its compatibility name, but returns the existing runtime.
// It never creates a draft, copies credentials, or replaces Docker resources.
// The account API checks ownership before invoking this resource.
func (s *Service) ReauthDraft(ctx context.Context, accountID int64) (string, bool, error) {
	if !s.runtimesOn(ctx) {
		return "", false, reasonError(core.ErrUnavailable, "not_configured")
	}
	d, err := s.desired(ctx, accountID, false)
	if store.IsNoRows(err) || (err == nil && d.Deleted) {
		return "", false, core.ErrNotFound
	}
	if err != nil {
		return "", false, err
	}
	if d.Kind != "managed" {
		return "", false, reasonError(core.ErrInvalidArgument, "api_key_account")
	}
	if reason := d.unavailable(); reason != "" {
		return "", false, reasonError(core.ErrInvalidArgument, reason)
	}
	if d.Parked {
		// Re-authorizing a stopped runtime starts it first.
		if err := s.WakeRuntime(ctx, accountID); err != nil {
			return "", false, err
		}
		if d, err = s.desired(ctx, accountID, false); err != nil {
			return "", false, err
		}
	}
	res, close, err := s.runtimeRequest(ctx, d.Key, http.MethodGet, "status", nil, "")
	if err != nil {
		return "", false, transportError(err)
	}
	defer close()
	defer res.Body.Close()
	var status struct {
		Status   string `json:"status"`
		Revision string `json:"revision"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&status) != nil || status.Status != "ready" || status.Revision != d.Revision {
		return "", false, reasonError(core.ErrUnavailable, "runtime_not_ready")
	}
	return d.Key, false, nil
}

// CommitReauth verifies login on the same runtime and resets account-side
// failure/quota state. Runtime mappings, sessions and volumes are retained.
func (s *Service) CommitReauth(ctx context.Context, accountID int64, key string, fn func(*gorm.DB, string) error) (string, error) {
	if !s.runtimesOn(ctx) {
		return "", reasonError(core.ErrUnavailable, "not_configured")
	}
	if !accountKeyPattern.MatchString(key) && !isDraftKey(key) {
		return "", draftNotFound(core.ErrInvalidArgument)
	}
	subscriptionChanged := false
	var observed core.AccountPreparationFacts
	err := s.lockedTx(ctx, key, true, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := g.Exec(reauthLockSQL, accountID).Error; err != nil {
			return err
		}
		// The runtime key of the managed account, which stays locked.
		var current string
		err := g.Table("accounts a").Joins("LEFT JOIN ccgateway_runtimes r ON r.account_id = a.id").
			Select("COALESCE(r.key, a.id::text)").
			Where("a.id = ? AND a.plugin_key = 'ccgateway' AND a.type = 'managed' AND a.deleted_at IS NULL", accountID).
			Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate, Table: clause.Table{Name: "a"}}).
			Take(&current).Error
		if store.IsNoRows(err) || (err == nil && current != key) {
			return draftNotFound(core.ErrInvalidArgument)
		}
		if err != nil {
			return err
		}
		d, err := s.desiredIn(ctx, tx, accountID, false)
		if err != nil {
			return err
		}
		if reason := d.unavailable(); reason != "" {
			return reasonError(core.ErrInvalidArgument, reason)
		}
		res, close, err := s.runtimeRequestIn(ctx, tx, key, http.MethodGet, "admin/status", nil, d.Revision)
		if err != nil {
			return transportError(err)
		}
		defer close()
		defer res.Body.Close()
		var health struct {
			LoggedIn         bool   `json:"logged_in"`
			SubscriptionType string `json:"subscription_type"`
			RateLimitTier    string `json:"rate_limit_tier"`
			AccountEmail     string `json:"account_email"`
		}
		if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&health) != nil || !health.LoggedIn {
			return reasonError(core.ErrInvalidArgument, "draft_not_authorized")
		}
		observed = observedFacts(health.SubscriptionType, health.AccountEmail)
		changed, err := recordSubscription(ctx, tx, accountID, health.SubscriptionType)
		if err != nil {
			return err
		}
		if err := recordRateLimitTier(ctx, tx, accountID, health.RateLimitTier); err != nil {
			return err
		}
		subscriptionChanged = changed
		if fn != nil {
			return fn(tx, "")
		}
		return nil
	})
	if err == nil {
		s.Kick(key)
		// Only once the new subscription type is committed (CONTRACTS §59);
		// a re-authorization may also be another login (its email).
		s.factsObserved(ctx, accountID, observed, subscriptionChanged)
	}
	return "", err
}

package ccgateway

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var subscriptionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// emailPattern bounds the login email a Worker reports: one address, no
// spaces or control characters.
var emailPattern = regexp.MustCompile(`^[^\s@]{1,64}@[^\s@]{1,255}$`)

// normalizeEmail keeps the email of the Claude login a Worker reported; ""
// for anything that is not one address.
func normalizeEmail(value string) string {
	if len(value) <= 320 && emailPattern.MatchString(value) {
		return value
	}
	return ""
}

// normalizeSubscription keeps a subscription type the Worker reported from
// Claude Code ("max", "pro", "team", "enterprise" ...); "" for anything else.
func normalizeSubscription(value string) string {
	if subscriptionPattern.MatchString(value) {
		return value
	}
	return ""
}

// Local credential presence is not a provider authorization verdict.
func safeAuthStatus(raw []byte) (any, error) {
	var v struct {
		Healthy                    bool     `json:"healthy"`
		LoggedIn                   bool     `json:"logged_in"`
		AuthMethod                 string   `json:"auth_method"`
		StatusSource               string   `json:"status_source,omitempty"`
		OnlineVerified             *bool    `json:"online_verified,omitempty"`
		SelectionVerified          *bool    `json:"selection_verified,omitempty"`
		CredentialPresent          *bool    `json:"credential_present,omitempty"`
		CredentialSources          []string `json:"credential_sources,omitempty"`
		CredentialSourceUnresolved *bool    `json:"credential_source_unresolved,omitempty"`
		AccessTokenExpired         *bool    `json:"access_token_expired,omitempty"`
		// SubscriptionType is what Claude Code reported for the signed-in
		// account (CONTRACTS §59); absent when unknown.
		SubscriptionType string `json:"subscription_type,omitempty"`
		// RateLimitTier is the login's tier as the upstream names it; absent
		// when unknown or from a Worker that does not report it.
		RateLimitTier string `json:"rate_limit_tier,omitempty"`
		// AccountEmail is the email of the signed-in Claude login; absent
		// when unknown.
		AccountEmail string `json:"account_email,omitempty"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil, errors.New("invalid status")
	}
	switch v.AuthMethod {
	case "oauth", "api_key", "claude_ai", "claude.ai", "oauth_token", "none", "", "unresolved", "third_party":
	default:
		v.AuthMethod = "unknown"
	}
	if v.StatusSource == "" { // Older Worker contract: retain only its original facts.
		return struct {
			Healthy    bool   `json:"healthy"`
			LoggedIn   bool   `json:"logged_in"`
			AuthMethod string `json:"auth_method"`
		}{v.Healthy, v.LoggedIn, v.AuthMethod}, nil
	}
	if v.StatusSource != "local_snapshot" || v.OnlineVerified == nil || *v.OnlineVerified || v.CredentialPresent == nil || v.SelectionVerified == nil || *v.SelectionVerified {
		return nil, errors.New("invalid local status facts")
	}
	seen := map[string]bool{}
	if *v.CredentialPresent && len(v.CredentialSources) == 0 {
		return nil, errors.New("credential presence lacks a known source")
	}
	for _, source := range v.CredentialSources {
		switch source {
		case "api_key_env", "bearer_env", "oauth_token_env", "stored_oauth", "managed_api_key", "external_credential_source", "credential_helper_configured":
		default:
			return nil, errors.New("invalid credential source")
		}
		if seen[source] {
			return nil, errors.New("duplicate credential source")
		}
		seen[source] = true
	}
	v.LoggedIn = *v.CredentialPresent
	v.SubscriptionType = normalizeSubscription(v.SubscriptionType)
	v.RateLimitTier = normalizeSubscription(v.RateLimitTier)
	v.AccountEmail = normalizeEmail(v.AccountEmail)
	return v, nil
}

// recordSubscription keeps on the account the subscription type Claude Code
// reported in its runtime (CONTRACTS §59). An unknown value ("") leaves the
// recorded one alone: older Workers do not report it. It reports whether the
// recorded value actually changed.
func recordSubscription(ctx context.Context, q *gorm.DB, accountID int64, value string) (bool, error) {
	if value = normalizeSubscription(value); value == "" || accountID <= 0 {
		return false, nil
	}
	res := recordAccountFact(store.G(ctx, q), accountID, "subscription_type", value)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// recordRateLimitTier keeps on the account the tier the runtime reported. An
// unknown value leaves the recorded one alone.
func recordRateLimitTier(ctx context.Context, q *gorm.DB, accountID int64, value string) error {
	if value = normalizeSubscription(value); value == "" || accountID <= 0 {
		return nil
	}
	return recordAccountFact(store.G(ctx, q), accountID, "rate_limit_tier", value).Error
}

// recordAccountFact sets column (a fact the runtime reports) of account
// accountID to value unless it holds that already, so an unchanged report
// writes no row. updated_at is left alone, as these updates always did.
func recordAccountFact(db *gorm.DB, accountID int64, column, value string) *gorm.DB {
	return db.Model(&model.Account{}).Where("id = ? AND ? IS DISTINCT FROM ?", accountID, clause.Column{Name: column}, value).Update(column, value)
}

// factsObserved runs the FactsObserved hook, when any, once what the
// runtime reported was committed.
func (s *Service) factsObserved(ctx context.Context, accountID int64, observed core.AccountPreparationFacts, subscriptionChanged bool) {
	if s.FactsObserved != nil && accountID > 0 {
		s.FactsObserved(ctx, accountID, observed, subscriptionChanged)
	}
}

// observedFacts are the facts of a Worker status answer.
func observedFacts(subscription, email string) core.AccountPreparationFacts {
	return core.AccountPreparationFacts{SubscriptionType: normalizeSubscription(subscription), AccountEmail: normalizeEmail(email)}
}

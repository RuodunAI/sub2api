package iam

import (
	"context"
	"encoding/json"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The purposes of an auth_flows row.
const (
	flowLogin2FA        = "login_2fa"
	flowTOTPSetup       = "totp_setup"
	flowPasskeyLogin    = "passkey_login"
	flowPasskeyRegister = "passkey_register"
)

const (
	// flowTTL bounds every flow, the WebAuthn ceremonies and the wait for a
	// code included.
	flowTTL = 5 * time.Minute
	// flowTTLSeconds is flowTTL in the API's expires_in.
	flowTTLSeconds = int64(flowTTL / time.Second)
)

var errFlowExpired = core.ErrInvalidArgument.WithMessage("this step has expired, start again")

// createFlow stores payload under a new random token and returns the token.
// userID 0 stores no owner (a discoverable passkey login does not know it yet).
// Expired rows are removed here, so the table never needs a cleanup job.
func (s *Service) createFlow(ctx context.Context, purpose string, userID int64, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	var owner *int64
	if userID != 0 {
		owner = &userID
	}
	g := store.G(ctx, s.db.Gorm)
	if err := deleteExpiredFlows(g).Error; err != nil {
		return "", err
	}
	if err := insertFlow(g, hashToken(token), purpose, owner, raw, time.Now().Add(flowTTL)).Error; err != nil {
		return "", err
	}
	return token, nil
}

// deleteExpiredFlows removes the flows that expired.
func deleteExpiredFlows(db *gorm.DB) *gorm.DB {
	return db.Where("expires_at < now()").Delete(&model.AuthFlow{})
}

// insertFlow stores a flow of owner (nil: none), raw: Create would add a
// RETURNING of the defaulted columns.
func insertFlow(db *gorm.DB, hash, purpose string, owner *int64, payload []byte, expiresAt time.Time) *gorm.DB {
	return db.Exec(`INSERT INTO auth_flows (token_hash, purpose, user_id, payload, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hash, purpose, owner, payload, expiresAt)
}

// liveFlow selects cols of the unexpired flow token of purpose.
func liveFlow(db *gorm.DB, token, purpose, cols string) *gorm.DB {
	return db.Model(&model.AuthFlow{}).Select(cols).Where("token_hash = ? AND purpose = ? AND expires_at > now()", hashToken(token), purpose)
}

// ownedFlow is liveFlow of the user userID only.
func ownedFlow(db *gorm.DB, token, purpose string, userID int64, cols string) *gorm.DB {
	return liveFlow(db, token, purpose, cols).Where("user_id = ?", userID)
}

// takeFlowQuery deletes the unexpired flow hash of purpose, of the user
// userID (0: of any owner), and returns its payload and owner.
func takeFlowQuery(db *gorm.DB, hash, purpose string, userID int64) *gorm.DB {
	return db.Raw(`DELETE FROM auth_flows WHERE token_hash = ? AND purpose = ? AND expires_at > now()
		AND (?::bigint = 0 OR user_id = ?) RETURNING payload, user_id`, hash, purpose, userID, userID)
}

// takeFlow consumes a flow: it can be taken once, by the user it belongs to
// (userID 0 takes a flow of any owner). out receives the payload; the returned
// id is the owner, 0 when none.
func (s *Service) takeFlow(ctx context.Context, q *gorm.DB, token, purpose string, userID int64, out any) (int64, error) {
	if token == "" {
		return 0, errFlowExpired
	}
	var raw []byte
	var owner *int64
	err := takeFlowQuery(store.G(ctx, q), hashToken(token), purpose, userID).Row().Scan(&raw, &owner)
	if store.IsNoRows(err) {
		return 0, errFlowExpired
	}
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, err
	}
	if owner == nil {
		return 0, nil
	}
	return *owner, nil
}

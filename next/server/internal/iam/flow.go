package iam

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Auth flows (CONTRACTS §79.1, §100.3) live in Valkey (tokenstore.FlowStore)
// once session.issue is s2s, and in auth_flows before: older nodes, which
// read only the table, must find the flows this node starts until every node
// reads both. Reads try Valkey first, then the table, so a flow started on
// either side of the switch finishes; a Valkey flow is taken with GETDEL.

// The purposes of a flow.
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
// A table flow is written through q, the caller's transaction or the pool.
// Expired rows are removed here, so the table never needs a cleanup job.
func (s *Service) createFlow(ctx context.Context, q *gorm.DB, purpose string, userID int64, payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if s.issuesOpaque(ctx) {
		return s.flows.PutFlow(ctx, purpose, tokenstore.Flow{UserID: userID, Payload: raw})
	}
	token, hash := tokenstore.NewToken("")
	var owner *int64
	if userID != 0 {
		owner = &userID
	}
	g := store.G(ctx, q)
	if err := deleteExpiredFlows(g).Error; err != nil {
		return "", err
	}
	if err := insertFlow(g, hash, purpose, owner, raw, time.Now().Add(flowTTL)).Error; err != nil {
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
	return db.Model(&model.AuthFlow{}).Select(cols).Where("token_hash = ? AND purpose = ? AND expires_at > now()", tokenstore.Hash(token), purpose)
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

// peekFlow reads a live flow without consuming it, of the user userID (0:
// of any owner). out receives the payload; the returned id is the owner, 0
// when none.
func (s *Service) peekFlow(ctx context.Context, token, purpose string, userID int64, out any) (int64, error) {
	return s.readFlow(ctx, token, purpose, userID, out, tokenstore.FlowStore.PeekFlow, func() *gorm.DB {
		g := store.G(ctx, s.db.Gorm)
		if userID != 0 {
			return ownedFlow(g, token, purpose, userID, "payload, user_id")
		}
		return liveFlow(g, token, purpose, "payload, user_id")
	})
}

// takeFlow consumes a flow: it can be taken once, by the user it belongs to
// (userID 0 takes a flow of any owner). out receives the payload; the
// returned id is the owner, 0 when none. A table flow is deleted through q,
// the caller's transaction or the pool.
func (s *Service) takeFlow(ctx context.Context, q *gorm.DB, token, purpose string, userID int64, out any) (int64, error) {
	return s.readFlow(ctx, token, purpose, userID, out, tokenstore.FlowStore.TakeFlow, func() *gorm.DB {
		return takeFlowQuery(store.G(ctx, q), tokenstore.Hash(token), purpose, userID)
	})
}

// readFlow reads a flow with fromValkey, then from the table with
// fromTable (a statement selecting payload and owner). A Valkey flow of
// another owner is no flow; taken, it is gone: knowing its token is enough
// to end it, as with any other bearer. With Valkey down a flow in the table
// (session.issue jwt) still finishes; one in neither fails with Valkey's
// error, as it may be there.
func (s *Service) readFlow(ctx context.Context, token, purpose string, userID int64, out any,
	fromValkey func(tokenstore.FlowStore, context.Context, string, string) (tokenstore.Flow, error), fromTable func() *gorm.DB) (int64, error) {
	if token == "" {
		return 0, errFlowExpired
	}
	var down error
	if s.flows != nil {
		f, err := fromValkey(s.flows, ctx, purpose, token)
		if err == nil {
			if userID != 0 && f.UserID != userID {
				return 0, errFlowExpired
			}
			return f.UserID, json.Unmarshal(f.Payload, out)
		}
		if !errors.Is(err, tokenstore.ErrNotFound) {
			down = err
		}
	}
	var raw []byte
	var owner *int64
	err := fromTable().Row().Scan(&raw, &owner)
	if store.IsNoRows(err) && down != nil {
		return 0, down
	}
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

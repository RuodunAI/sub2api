package iam

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
)

// Auth flows (CONTRACTS §79.1, §100.3) live in Valkey (tokenstore.FlowStore):
// shared by the nodes, a peek is a GET and a take a GETDEL, so only one
// request gets a flow. Valkey down fails them with 503.

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
func (s *Service) createFlow(ctx context.Context, purpose string, userID int64, payload any) (string, error) {
	if s.flows == nil {
		return "", tokenstore.ErrUnavailable
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return s.flows.PutFlow(ctx, purpose, tokenstore.Flow{UserID: userID, Payload: raw})
}

// peekFlow reads a live flow without consuming it, of the user userID (0:
// of any owner). out receives the payload; the returned id is the owner, 0
// when none.
func (s *Service) peekFlow(ctx context.Context, token, purpose string, userID int64, out any) (int64, error) {
	return s.readFlow(ctx, token, purpose, userID, out, tokenstore.FlowStore.PeekFlow)
}

// takeFlow consumes a flow: it can be taken once, by the user it belongs to
// (userID 0 takes a flow of any owner). out receives the payload; the
// returned id is the owner, 0 when none.
func (s *Service) takeFlow(ctx context.Context, token, purpose string, userID int64, out any) (int64, error) {
	return s.readFlow(ctx, token, purpose, userID, out, tokenstore.FlowStore.TakeFlow)
}

// readFlow reads a flow with read. A flow of another owner is no flow;
// taken, it is gone: knowing its token is enough to end it, as with any
// other bearer.
func (s *Service) readFlow(ctx context.Context, token, purpose string, userID int64, out any,
	read func(tokenstore.FlowStore, context.Context, string, string) (tokenstore.Flow, error)) (int64, error) {
	if token == "" {
		return 0, errFlowExpired
	}
	if s.flows == nil {
		return 0, tokenstore.ErrUnavailable
	}
	f, err := read(s.flows, ctx, purpose, token)
	if errors.Is(err, tokenstore.ErrNotFound) {
		return 0, errFlowExpired
	}
	if err != nil {
		return 0, err
	}
	if userID != 0 && f.UserID != userID {
		return 0, errFlowExpired
	}
	return f.UserID, json.Unmarshal(f.Payload, out)
}

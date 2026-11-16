// Package tokenstore keeps what a console login leaves behind in Valkey
// (CONTRACTS §100): the session tokens (s2s_), and the short auth flows
// (two-factor login, passkey ceremonies, TOTP enrolment, step-up proofs and
// windows).
//
// Only SHA-256 hashes of tokens are stored, so a copy of Valkey does not sign
// anyone in. Valkey is the source of truth for both: there is no fallback when
// it is down (the operator's decision), every call then fails with
// ErrUnavailable, which renders as 503.
package tokenstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// SessionPrefix starts every session token, so a bearer token is told from
// an access key (core.AccessKeyPrefix), or anything else, without a lookup.
const SessionPrefix = "s2s_"

var (
	// ErrNotFound: the token names no live session or flow (never issued,
	// expired, revoked or already taken).
	ErrNotFound = errors.New("tokenstore: not found")
	// ErrSuperseded: the token a rotation starts from is no longer its
	// session's current one (Rotate).
	ErrSuperseded = errors.New("tokenstore: token superseded by a rotation")
	// ErrUnavailable: Valkey did not answer. Sessions cannot be checked or
	// issued without it, and nothing stands in for it.
	ErrUnavailable = core.ErrUnavailable.WithMessage("the session store is unavailable, try again shortly")
)

// Session is one signed-in browser or device.
type Session struct {
	// ID is the login session (core.Session): step-up proofs bind to it and
	// the sessions list marks the caller's own by it. A rotation keeps it.
	ID           string
	UserID       int64
	Method       string
	IP           string
	UserAgent    string
	TokenVersion int // users.token_version when issued, or restamped since
	CreatedAt    time.Time
	LastSeen     time.Time
}

// Selection picks sessions of a user: Only that one, or every one but Keep;
// the zero value picks them all.
type Selection struct{ Only, Keep string }

// Bump is the users.token_version bump a revocation goes with, From the
// version before it To the one after; the zero value is none. A session
// stamped below From was ended by an earlier bump that did not reach
// Valkey (rejected when presented, still stored): it ends now, and is never
// stamped back to life. One stamped From or more is kept at To or above.
type Bump struct{ From, To int }

// SessionStore keeps the sessions.
type SessionStore interface {
	// Create stores s (CreatedAt and LastSeen are set to now) under a new
	// token and returns the token.
	Create(ctx context.Context, s Session) (string, error)
	// Lookup resolves a token; ErrNotFound when it names no live session.
	Lookup(ctx context.Context, token string) (Session, error)
	// Touch records that s, found by token, was just used, from s.IP with
	// s.UserAgent, extending its idle timeout; it writes only when s was
	// last seen a while ago.
	Touch(ctx context.Context, token string, s Session) error
	// Rotate gives the session id of userID a new token in place of
	// presented, the one the caller holds. The old one keeps working for a
	// short grace, for the requests already sent with it. ErrNotFound when
	// the user has no such session; ErrSuperseded when presented is no
	// longer its current token (a rotation since left it in its grace):
	// the newer token is out already, and rotating it would end it.
	Rotate(ctx context.Context, userID int64, id, presented string) (string, error)
	// List returns the live sessions of userID, most recently used first,
	// dropping index entries whose session is gone.
	List(ctx context.Context, userID int64) ([]Session, error)
	// Has reports whether userID has the live session id stamped
	// tokenVersion or above: one stamped below was ended by a bump.
	Has(ctx context.Context, userID int64, id string, tokenVersion int) (bool, error)
	// Revoke ends the sessions of userID that sel picks at once. With a
	// bump it also ends the others stamped below b.From and stamps the rest
	// below b.To with b.To; without one, Only touches that session alone.
	// With a bump, the sessions issued after the call are left as they
	// are, should Valkey run it late (after the caller gave up on it): one
	// stamped above b.To, or created since and stamped below b.To. Neither
	// can be a session from before the call that the bump would not end.
	Revoke(ctx context.Context, userID int64, sel Selection, b Bump) error
	// Prune ends the sessions of userID stamped below tokenVersion: those a
	// bump ended without reaching Valkey. Later sessions stay.
	Prune(ctx context.Context, userID int64, tokenVersion int) error
	// Drop ends what token, found as s stamped below version, still opens:
	// its whole session when it is the session's current token, only
	// itself when a rotation left it behind (the session's current token
	// is not ended by a request that an old one sent). False when token is
	// stamped version or above by now (a bump that kept it restamped it
	// after it was read): it stays, and is not ended.
	Drop(ctx context.Context, token string, s Session, version int) (bool, error)
}

// Flow is a stored auth flow.
type Flow struct {
	UserID  int64 // 0: no owner yet (a discoverable passkey login)
	Payload []byte
}

// FlowStore keeps the short auth flows and the step-up windows.
type FlowStore interface {
	// PutFlow stores a flow of purpose under a new token and returns it.
	PutFlow(ctx context.Context, purpose string, f Flow) (string, error)
	// PeekFlow reads a live flow without taking it.
	PeekFlow(ctx context.Context, purpose, token string) (Flow, error)
	// TakeFlow reads and deletes a live flow at once: only one caller gets it.
	TakeFlow(ctx context.Context, purpose, token string) (Flow, error)
}

// NewToken returns a random token with prefix and the hash it is stored by
// (Hash): 32 random bytes, base64url. Access keys and flow tokens are made
// the same way.
func NewToken(prefix string) (token, hash string) {
	var b [32]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand since Go 1.24)
	token = prefix + base64.RawURLEncoding.EncodeToString(b[:])
	return token, Hash(token)
}

// Hash is the key a token is stored by: hex SHA-256, as the access keys
// are hashed in PostgreSQL.
func Hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// IsUnavailable reports whether err is Valkey not answering (ErrUnavailable).
func IsUnavailable(err error) bool {
	var e *core.Error
	return errors.As(err, &e) && e.Code == ErrUnavailable.Code
}

// unavailable wraps a Valkey failure. It is logged here at error level, the
// alert of an outage that signs every console user out of reach.
func unavailable(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, "tokenstore: valkey unavailable", "op", op, "err", err)
	return ErrUnavailable.WithCause(err)
}

package iam

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Passkey is the API view of a registered passkey.
type Passkey struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	RPID       string     `json:"rp_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// PasskeyCeremony is the response of the two begin endpoints: the options for
// the browser's WebAuthn call.
type PasskeyCeremony struct {
	FlowToken string `json:"flow_token"`
	ExpiresIn int64  `json:"expires_in"`
	Options   any    `json:"options"`
}

var errPasskeyUnavailable = core.ErrInvalidArgument.WithMessage("passkeys are not available on this address")
var errPasskeyFailed = core.ErrUnauthenticated.WithMessage("passkey verification failed")

// webAuthnFor builds the relying party of a request. SUB2API_WEBAUTHN_RP_ID
// names it and the browser's Origin must be that domain or below it. Without
// the setting it is the host the console was opened on, which the Origin must
// name; a failover proxy that rewrites Host therefore needs the setting. The
// browser signs the origin it really is on, and that is what the library
// compares with this one, so a request with a forged Origin header gets no
// further than one with a true one.
func (s *Service) webAuthnFor(r *http.Request, rpID string) (*webauthn.WebAuthn, error) {
	origin := r.Header.Get("Origin")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || u.Hostname() == "" {
		return nil, errPasskeyUnavailable
	}
	host := u.Hostname()
	if rpID == "" {
		rpID = s.cfg.WebAuthnRPID
	}
	if s.cfg.WebAuthnRPID == "" {
		requestHost := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			requestHost = h
		}
		if !strings.EqualFold(host, requestHost) {
			return nil, errPasskeyUnavailable
		}
	}
	if rpID == "" {
		rpID = host
	}
	if !strings.EqualFold(host, rpID) && !strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(rpID)) {
		return nil, errPasskeyUnavailable
	}
	// WebAuthn has no IP-address relying parties.
	if net.ParseIP(rpID) != nil {
		return nil, errPasskeyUnavailable
	}
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: totpIssuer,
		RPOrigins:     []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification:   protocol.VerificationRequired,
		},
	})
}

// webAuthnUser adapts a user to the library.
type webAuthnUser struct {
	id          int64
	email       string
	displayName string
	credentials []webauthn.Credential
}

func userHandle(id int64) []byte { return []byte(strconv.FormatInt(id, 10)) }

func (u *webAuthnUser) WebAuthnID() []byte                         { return userHandle(u.id) }
func (u *webAuthnUser) WebAuthnName() string                       { return u.email }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return strOr(u.displayName, u.email) }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func strOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Service) loadWebAuthnUser(ctx context.Context, userID int64) (*webAuthnUser, string, error) {
	u := &webAuthnUser{id: userID}
	var status string
	g := store.G(ctx, s.db.Pool)
	err := liveUser(g, userID, "email, display_name, status").Row().Scan(&u.email, &u.displayName, &status)
	if store.IsNoRows(err) {
		return nil, "", errPasskeyFailed
	}
	if err != nil {
		return nil, "", err
	}
	var rows []model.UserPasskey
	if err := passkeyCredentials(g, userID).Find(&rows).Error; err != nil {
		return nil, "", err
	}
	for _, r := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(r.Credential, &c); err != nil {
			return nil, "", err
		}
		u.credentials = append(u.credentials, c)
	}
	return u, status, nil
}

// passkeysOf are the passkeys of userID.
func passkeysOf(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.UserPasskey{}).Where("user_id = ?", userID)
}

// passkeyCredentials reads the WebAuthn credentials of userID.
func passkeyCredentials(db *gorm.DB, userID int64) *gorm.DB {
	return passkeysOf(db, userID).Select("credential")
}

// Passkeys lists the user's passkeys.
func (s *Service) Passkeys(ctx context.Context, userID int64) ([]Passkey, error) {
	var rows []model.UserPasskey
	if err := passkeyList(store.G(ctx, s.db.Pool), userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Passkey, 0, len(rows))
	for _, r := range rows {
		out = append(out, Passkey{ID: r.ID, Name: r.Name, RPID: r.RpID, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt})
	}
	return out, nil
}

// passkeyList is what Passkeys shows of the passkeys of userID, newest first.
func passkeyList(db *gorm.DB, userID int64) *gorm.DB {
	return passkeysOf(db, userID).Select("id, name, rp_id, created_at, last_used_at").Order("created_at DESC")
}

// normalizePasskeyName trims a passkey's name; empty means "Passkey".
func normalizePasskeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 100 {
		return "", core.InvalidFields(core.FieldError{Field: "name", Code: "too_long", Message: "at most 100 characters"})
	}
	return name, nil
}

// BeginPasskeyRegistration starts adding a passkey; the route has verified
// the caller (step-up self.passkey.add).
func (s *Service) BeginPasskeyRegistration(ctx context.Context, r *http.Request, userID int64) (*PasskeyCeremony, error) {
	wa, err := s.webAuthnFor(r, "")
	if err != nil {
		return nil, err
	}
	u, _, err := s.loadWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(u.credentials))
	for _, c := range u.credentials {
		exclude = append(exclude, c.Descriptor())
	}
	options, session, err := wa.BeginRegistration(u, webauthn.WithExclusions(exclude))
	if err != nil {
		return nil, err
	}
	token, err := s.createFlow(ctx, flowPasskeyRegister, userID, session)
	if err != nil {
		return nil, err
	}
	return &PasskeyCeremony{FlowToken: token, ExpiresIn: flowTTLSeconds, Options: options}, nil
}

// FinishPasskeyRegistration verifies the browser's answer and stores the passkey.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, r *http.Request, userID int64, flowToken, name string, credential json.RawMessage) (*Passkey, error) {
	name, err := normalizePasskeyName(name)
	if err != nil {
		return nil, err
	}
	var session webauthn.SessionData
	if _, err := s.takeFlow(ctx, s.db.Pool, flowToken, flowPasskeyRegister, userID, &session); err != nil {
		return nil, err
	}
	wa, err := s.webAuthnFor(r, session.RelyingPartyID)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(credential)
	if err != nil {
		return nil, core.ErrInvalidArgument.WithMessage("invalid passkey response").WithCause(err)
	}
	u, _, err := s.loadWebAuthnUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	c, err := wa.CreateCredential(u, session, parsed)
	if err != nil {
		return nil, core.ErrInvalidArgument.WithMessage("passkey verification failed").WithCause(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	p := &Passkey{Name: name, RPID: session.RelyingPartyID}
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		err := store.G(ctx, tx).Raw(insertPasskey, userID, name, session.RelyingPartyID, c.ID, raw).Row().Scan(&p.ID, &p.CreatedAt)
		if store.IsNoRows(err) {
			return core.ErrConflict.WithMessage("this passkey is already registered")
		}
		if err != nil {
			return err
		}
		return audit.Audit(ctx, tx, userID, "user.passkey.add", "user", strconv.FormatInt(userID, 10), map[string]any{"passkey_id": p.ID, "name": name})
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// insertPasskey stores a new passkey; a credential stored before returns no
// row. credential_id and credential are bytes: none of their ? follows a (.
const insertPasskey = `INSERT INTO user_passkeys (user_id, name, rp_id, credential_id, credential) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (credential_id) DO NOTHING RETURNING id, created_at`

// DeletePasskey removes one of the user's passkeys.
func (s *Service) DeletePasskey(ctx context.Context, userID, id int64) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := s.keepLastFactor(ctx, tx, userID, core.StepUpSelfPasskeyDelete); err != nil {
			return err
		}
		res := deletePasskey(store.G(ctx, tx), id, userID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return core.ErrNotFound
		}
		return audit.Audit(ctx, tx, userID, "user.passkey.delete", "user", strconv.FormatInt(userID, 10), map[string]any{"passkey_id": id})
	})
}

// deletePasskey removes the passkey id of userID.
func deletePasskey(db *gorm.DB, id, userID int64) *gorm.DB {
	return db.Where("id = ? AND user_id = ?", id, userID).Delete(&model.UserPasskey{})
}

// BeginPasskeyLogin starts a discoverable login: the browser offers the
// passkeys it has for this site, so no email is asked for or revealed.
func (s *Service) BeginPasskeyLogin(ctx context.Context, r *http.Request, ip string) (*PasskeyCeremony, error) {
	if wait := s.limiter.check(ctx, "", ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	if wait := s.limiter.allowPasskeyBegin(ctx, ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	wa, err := s.webAuthnFor(r, "")
	if err != nil {
		return nil, err
	}
	options, session, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, err
	}
	token, err := s.createFlow(ctx, flowPasskeyLogin, 0, session)
	if err != nil {
		return nil, err
	}
	return &PasskeyCeremony{FlowToken: token, ExpiresIn: flowTTLSeconds, Options: options}, nil
}

// FinishPasskeyLogin verifies the assertion and issues tokens. A passkey checks
// the user's presence and verification itself, so two-factor is not asked for
// again. Every failure looks the same and counts towards the login rate limit.
func (s *Service) FinishPasskeyLogin(ctx context.Context, r *http.Request, flowToken string, credential json.RawMessage, ip string) (*TokenPair, error) {
	if wait := s.limiter.check(ctx, "", ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	var session webauthn.SessionData
	if _, err := s.takeFlow(ctx, s.db.Pool, flowToken, flowPasskeyLogin, 0, &session); err != nil {
		return nil, err
	}
	fail := func() (*TokenPair, error) {
		s.limiter.fail(ctx, "", ip)
		return nil, errPasskeyFailed
	}
	wa, err := s.webAuthnFor(r, session.RelyingPartyID)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(credential)
	if err != nil {
		return fail()
	}
	var user *webAuthnUser
	var passkeyID int64
	found, verified, err := wa.ValidatePasskeyLogin(func(rawID, handle []byte) (webauthn.User, error) {
		var owner int64
		var rpID string
		err := passkeyByCredential(store.G(ctx, s.db.Pool), rawID).Row().Scan(&passkeyID, &owner, &rpID)
		if err != nil {
			return nil, err
		}
		if rpID != session.RelyingPartyID || !bytes.Equal(handle, userHandle(owner)) {
			return nil, errPasskeyFailed
		}
		u, status, err := s.loadWebAuthnUser(ctx, owner)
		if err != nil {
			return nil, err
		}
		if status != StatusActive {
			return nil, errPasskeyFailed
		}
		user = u
		return u, nil
	}, session, parsed)
	if err != nil || found == nil || user == nil {
		return fail()
	}
	raw, err := json.Marshal(verified)
	if err != nil {
		return nil, err
	}
	var pair *TokenPair
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		g := store.G(ctx, tx)
		var status string
		if err := liveUser(g, user.id, "status").Clauses(store.ForUpdate).Row().Scan(&status); err != nil {
			if store.IsNoRows(err) {
				return errPasskeyFailed
			}
			return err
		}
		if status != StatusActive {
			return errPasskeyFailed
		}
		res := usePasskey(g.Where("id = ? AND user_id = ?", passkeyID, user.id), raw)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errPasskeyFailed
		}
		if err := touchLastLogin(g, user.id).Error; err != nil {
			return err
		}
		var err error
		pair, err = s.issueTokens(withLoginMethod(ctx, loginMethodPasskey), tx, user.id, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// passkeyByCredential reads the passkey of a credential the browser names.
func passkeyByCredential(db *gorm.DB, credentialID []byte) *gorm.DB {
	return db.Model(&model.UserPasskey{}).Select("id, user_id, rp_id").Where("credential_id = ?", credentialID)
}

// usePasskey stores the credential of the passkey where selects as just
// verified (its sign count) and when it was used.
func usePasskey(where *gorm.DB, credential []byte) *gorm.DB {
	return where.Model(&model.UserPasskey{}).Updates(map[string]any{"credential": credential, "last_used_at": gorm.Expr("now()")})
}

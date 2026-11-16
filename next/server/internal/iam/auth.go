package iam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// TokenPair is the response of login and refresh. When the account has
// two-factor authentication, a correct password answers with only
// TwoFactorRequired and FlowToken; POST /auth/login/2fa gives the tokens.
// An opaque session (session.issue s2s) has no refresh token: the console
// then takes a 401 for the end of the session.
type TokenPair struct {
	AccessToken       string `json:"access_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	ExpiresIn         int64  `json:"expires_in,omitempty"` // access token lifetime (opaque: idle timeout), seconds
	User              *User  `json:"user,omitempty"`
	TwoFactorRequired bool   `json:"two_factor_required,omitempty"`
	FlowToken         string `json:"flow_token,omitempty"`
}

var (
	errBadCredentials = core.ErrUnauthenticated.WithMessage("invalid email or password")
	errUserDisabled   = core.ErrPermissionDenied.WithMessage("this account is disabled")
	errBadRefresh     = core.ErrUnauthenticated.WithMessage("invalid or expired refresh token")
)

// Login checks the password and issues tokens. Disabled users are rejected.
// ip is the client address used by the failed-login rate limit (CONTRACTS
// §14.2); while email+IP or IP is locked out the password is not checked.
func (s *Service) Login(ctx context.Context, email, password, ip string) (*TokenPair, error) {
	if wait := s.limiter.check(ctx, email, ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	var id int64
	var hash, status string
	err := userByEmail(store.G(ctx, s.db.Gorm), strings.ToLower(strings.TrimSpace(email)), "id, password_hash, status").Row().Scan(&id, &hash, &status)
	if store.IsNoRows(err) {
		// Burn comparable time so response timing does not reveal accounts.
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		s.limiter.fail(ctx, email, ip)
		return nil, errBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.limiter.fail(ctx, email, ip)
		return nil, errBadCredentials
	}
	s.limiter.reset(ctx, email, ip)
	if status != StatusActive {
		return nil, errUserDisabled
	}
	twoFactor, err := hasTwoFactor(store.G(ctx, s.db.Gorm), id)
	if err != nil {
		return nil, err
	}
	if twoFactor {
		token, err := s.createFlow(ctx, s.db.Gorm, flowLogin2FA, id, login2FAPayload{Email: limiterEmail(email), Password: tokenstore.Hash(hash)})
		if err != nil {
			return nil, err
		}
		return &TokenPair{TwoFactorRequired: true, FlowToken: token, ExpiresIn: flowTTLSeconds}, nil
	}
	var pair *TokenPair
	err = s.issueTx(ctx, func(is *issuer) error {
		// The user row is the parent lock for every refresh-token family.
		// Recheck the password snapshot after acquiring it: a password change
		// may have committed while bcrypt was running above.
		g := store.G(ctx, is.tx)
		var currentHash, currentStatus string
		if err := lockLiveUser(g, id, "password_hash, status").Row().Scan(&currentHash, &currentStatus); err != nil {
			if store.IsNoRows(err) {
				return errBadCredentials
			}
			return err
		}
		if currentHash != hash {
			return errBadCredentials
		}
		if currentStatus != StatusActive {
			return errUserDisabled
		}
		if err := touchLastLogin(g, id).Error; err != nil {
			return err
		}
		pair, err = is.issue(ctx, id, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	return pair, nil
}

// login2FAPayload is what a correct password leaves for the second step: the
// password snapshot, so a password change in between ends the login.
type login2FAPayload struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginSecondFactor finishes a password login with a TOTP or backup code. The
// flow survives a wrong code, under the per-user lockout and the login rate
// limit, and ends with the first correct one.
func (s *Service) LoginSecondFactor(ctx context.Context, flowToken, code, ip string) (*TokenPair, error) {
	var payload login2FAPayload
	userID, err := s.peekFlow(ctx, flowToken, flowLogin2FA, 0, &payload)
	if err != nil {
		return nil, err
	}
	if wait := s.limiter.check(ctx, payload.Email, ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	var pair *TokenPair
	err = s.issueTx(ctx, func(is *issuer) error {
		g := store.G(ctx, is.tx)
		var hash, status string
		if err := lockLiveUser(g, userID, "password_hash, status").Row().Scan(&hash, &status); err != nil {
			if store.IsNoRows(err) {
				return errBadCredentials
			}
			return err
		}
		if tokenstore.Hash(hash) != payload.Password {
			return errFlowExpired
		}
		if status != StatusActive {
			return errUserDisabled
		}
		if err := s.checkSecondFactor(ctx, is.tx, userID, code); err != nil {
			return err
		}
		if err := touchLastLogin(g, userID).Error; err != nil {
			return err
		}
		var err error
		if pair, err = is.issue(withLoginMethod(ctx, loginMethodTwoFactor), userID, ""); err != nil {
			return err
		}
		// Taken last: a failure before leaves the flow for another try (a
		// flow in Valkey does not come back with a rollback).
		var used login2FAPayload
		_, err = s.takeFlow(ctx, is.tx, flowToken, flowLogin2FA, userID, &used)
		return err
	})
	if isBadSecondFactor(err) {
		s.limiter.fail(ctx, payload.Email, ip)
		if ferr := s.recordSecondFactorFailure(ctx, userID); ferr != nil {
			return nil, ferr
		}
	}
	if err != nil {
		return nil, err
	}
	s.limiter.reset(ctx, payload.Email, ip)
	return pair, nil
}

// newFamilyID returns a random refresh token family id.
func newFamilyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// issuer issues credentials in one transaction (issueTx), in the mode
// session.issue says when it starts: read once, so a refresh decides on
// the exchange and issues by the same answer.
type issuer struct {
	s      *Service
	tx     *gorm.DB
	opaque bool
	// stored is the opaque session issued, which issueTx ends when the
	// transaction fails.
	stored *tokenstore.Session
}

// issueTx runs fn in a transaction, in which it issues credentials through
// the issuer it gets. An opaque session lives in Valkey, which does not
// roll back with PostgreSQL: when the transaction fails after storing one,
// it is ended, so no session nobody holds stays on the user's list.
func (s *Service) issueTx(ctx context.Context, fn func(is *issuer) error) error {
	is := &issuer{s: s, opaque: s.issuesOpaque(ctx)}
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		is.tx = tx
		return fn(is)
	})
	if err == nil || is.stored == nil {
		return err
	}
	uid := is.stored.UserID
	if rerr := s.sessions.Revoke(context.WithoutCancel(ctx), uid, tokenstore.Selection{Only: is.stored.ID}, tokenstore.Bump{}); rerr != nil {
		slog.WarnContext(ctx, "iam: ending an opaque session of a failed login failed", "user_id", uid, "err", rerr)
	}
	return err
}

// issue issues the credentials of a login session familyID of userID (a
// new one when empty, i.e. on login, which is audited for a superuser): an
// opaque session or a JWT pair. The caller holds the user's row lock.
func (is *issuer) issue(ctx context.Context, userID int64, familyID string) (*TokenPair, error) {
	s, g := is.s, store.G(ctx, is.tx)
	var tokenVersion int
	if err := userTokenVersion(g, userID).Row().Scan(&tokenVersion); err != nil {
		return nil, err
	}
	newFamily := familyID == ""
	if newFamily {
		var err error
		if familyID, err = newFamilyID(); err != nil {
			return nil, err
		}
		if err := s.auditSuperuserLogin(ctx, is.tx, userID); err != nil {
			return nil, err
		}
	}
	// The user is read first: an opaque session, stored in Valkey, is the
	// last thing done before the commit.
	user, err := s.loginUser(ctx, is.tx, userID)
	if err != nil {
		return nil, err
	}
	var pair *TokenPair
	if is.opaque {
		var stored tokenstore.Session
		if pair, stored, err = s.issueOpaque(ctx, userID, tokenVersion, familyID); err == nil {
			is.stored = &stored
		}
	} else {
		pair, err = s.issueJWT(ctx, g, userID, tokenVersion, familyID, newFamily)
	}
	if err != nil {
		return nil, err
	}
	pair.User = user
	return pair, nil
}

// loginUser is the user a login or refresh answers with: no balance or
// segment.
func (s *Service) loginUser(ctx context.Context, tx *gorm.DB, userID int64) (*User, error) {
	user, err := s.getUser(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	user.Balance = nil
	user.Segment = core.SegmentRef{}
	return user, nil
}

// issueJWT issues an access token and a refresh token in familyID.
func (s *Service) issueJWT(ctx context.Context, g *gorm.DB, userID int64, tokenVersion int, familyID string, isNew bool) (*TokenPair, error) {
	refresh, hash := tokenstore.NewToken("")
	access, err := s.issueAccessToken(userID, tokenVersion, familyID)
	if err != nil {
		return nil, err
	}
	if err := s.touchLoginSession(ctx, g, userID, familyID, isNew); err != nil {
		return nil, err
	}
	if err := insertRefreshToken(g, userID, hash, time.Now().Add(s.cfg.RefreshTokenTTL), familyID).Error; err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresIn: int64(s.cfg.AccessTokenTTL / time.Second)}, nil
}

// insertRefreshToken stores a refresh token, raw: Create would add a
// RETURNING of the defaulted columns.
func insertRefreshToken(db *gorm.DB, userID int64, hash string, expiresAt time.Time, familyID string) *gorm.DB {
	return db.Exec(`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, family_id) VALUES (?, ?, ?, ?)`, userID, hash, expiresAt, familyID)
}

// refreshTokens are the refresh_tokens rows.
func refreshTokens(db *gorm.DB) *gorm.DB { return db.Model(&model.RefreshToken{}) }

// userTokenVersion reads the token_version of userID, deleted or not.
func userTokenVersion(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.User{}).Select("token_version").Where("id = ?", userID)
}

// refreshByHash is the refresh token whose hash is hash.
func refreshByHash(db *gorm.DB, hash string) *gorm.DB {
	return refreshTokens(db).Where("token_hash = ?", hash)
}

// refreshOwner reads the user of the refresh token hash.
func refreshOwner(db *gorm.DB, hash string) *gorm.DB {
	return refreshByHash(db, hash).Select("user_id")
}

// lockRefreshToken reads the refresh token hash, locked until the
// transaction ends.
func lockRefreshToken(db *gorm.DB, hash string) *gorm.DB {
	return refreshByHash(db, hash).Select("id, user_id, family_id, revoked_at, replaced_at, expires_at").Clauses(store.ForUpdate)
}

// liveFamily are the live tokens of a refresh token family.
func liveFamily(db *gorm.DB, family string) *gorm.DB {
	return refreshTokens(db).Where("family_id = ? AND revoked_at IS NULL AND expires_at > now()", family)
}

// revokeRefresh revokes the live tokens that where selects.
func revokeRefresh(where *gorm.DB) *gorm.DB {
	return where.Update("revoked_at", gorm.Expr("now()"))
}

// revokeToken revokes the refresh token id, unless it is.
func revokeToken(db *gorm.DB, id int64) *gorm.DB {
	return revokeRefresh(refreshTokens(db).Where("id = ? AND revoked_at IS NULL", id))
}

// revokeFamily revokes the tokens of a refresh token family not revoked yet.
func revokeFamily(db *gorm.DB, family string) *gorm.DB {
	return revokeRefresh(refreshTokens(db).Where("family_id = ? AND revoked_at IS NULL", family))
}

// refreshVerdict is the outcome of presenting a stored refresh token.
type refreshVerdict int

const (
	refreshValid    refreshVerdict = iota
	refreshExpired                 // past expires_at, never used: plain 401
	refreshReplayed                // already rotated or revoked: revoke the family
)

// classifyRefresh decides what presenting a stored refresh token means. A
// token that was rotated (replaced_at) or revoked (revoked_at) must never be
// presented again; seeing it means it leaked, so the whole family is revoked
// even when the token has expired since.
func classifyRefresh(revokedAt, replacedAt *time.Time, expiresAt, now time.Time) refreshVerdict {
	if revokedAt != nil || replacedAt != nil {
		return refreshReplayed
	}
	if !expiresAt.After(now) {
		return refreshExpired
	}
	return refreshValid
}

// refreshReuseGrace is how long a rotated refresh token is still accepted
// (CONTRACTS §14.2): a console that never received the rotation's response
// presents the old token again moments later.
const refreshReuseGrace = time.Minute

// rotatedJustNow reports a token that was rotated (not logged out or revoked
// as a family) less than refreshReuseGrace ago.
func rotatedJustNow(revokedAt, replacedAt *time.Time, now time.Time) bool {
	return replacedAt != nil && revokedAt != nil && now.Sub(*replacedAt) < refreshReuseGrace
}

// Refresh rotates a refresh token: the presented one is marked revoked and
// replaced, and a new pair is issued in the same family. Presenting a token
// that was already rotated or revoked revokes every token of its family
// (CONTRACTS §14.2), and the opaque session it may have been exchanged for,
// and fails with 401. With session.issue s2s the token is exchanged for an
// opaque session of the same id instead (retireFamily): the user stays
// signed in across the switch, and the step-up proofs bound to the id stay
// valid. A token presented again within the grace of its rotation is
// answered by retryRefresh.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	if refreshToken == "" {
		return nil, errBadRefresh
	}
	var pair *TokenPair
	var replayed *presentedRefresh
	err := s.issueTx(ctx, func(is *issuer) error {
		t, err := lockPresentedRefresh(ctx, is.tx, refreshToken)
		if err != nil {
			return err
		}
		g := store.G(ctx, is.tx)
		now := time.Now()
		verdict := classifyRefresh(t.revokedAt, t.replacedAt, t.expiresAt, now)
		if verdict == refreshReplayed && t.family != "" && rotatedJustNow(t.revokedAt, t.replacedAt, now) {
			if verdict, pair, err = s.retryRefresh(ctx, is.tx, t); err != nil || pair != nil {
				return err
			}
		}
		switch verdict {
		case refreshReplayed:
			// Committed; the 401 is returned after the transaction.
			replayed = &t
			return s.endReplayedFamily(ctx, g, t)
		case refreshExpired:
			return errBadRefresh
		}
		if err := rotateRefresh(g, t.id).Error; err != nil {
			return err
		}
		if err := activeUser(g, t.userID); err != nil {
			return err
		}
		ictx := ctx
		if is.opaque {
			if ictx, err = retireFamily(ctx, g, t.userID, t.family); err != nil {
				return err
			}
		}
		pair, err = is.issue(ictx, t.userID, t.family)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replayed != nil {
		slog.WarnContext(ctx, "iam: refresh token reuse detected, token family revoked",
			"user_id", replayed.userID, "family_id", replayed.family, "token_id", replayed.id)
		// Ending the opaque session may have bumped token_version in
		// Valkey's place: the nodes must not wait out their status cache.
		s.sessionsChanged(ctx, replayed.userID)
		return nil, errBadRefresh
	}
	return pair, nil
}

// presentedRefresh is a refresh token presented to Refresh, read locked.
type presentedRefresh struct {
	id, userID            int64
	family                string
	revokedAt, replacedAt *time.Time
	expiresAt             time.Time
}

// lockPresentedRefresh reads refreshToken in tx under its user's row lock.
// All issuers and revokers serialize on the user: the parent is found
// without locking a child first, then a fresh snapshot of the token is
// read, locked.
func lockPresentedRefresh(ctx context.Context, tx *gorm.DB, refreshToken string) (presentedRefresh, error) {
	g := store.G(ctx, tx)
	hash := tokenstore.Hash(refreshToken)
	var t presentedRefresh
	err := refreshOwner(g, hash).Row().Scan(&t.userID)
	if err == nil {
		err = lockRefreshUser(ctx, tx, t.userID)
	}
	if err == nil {
		err = lockRefreshToken(g, hash).Row().Scan(&t.id, &t.userID, &t.family, &t.revokedAt, &t.replacedAt, &t.expiresAt)
	}
	if store.IsNoRows(err) {
		return t, errBadRefresh
	}
	return t, err
}

// retryRefresh answers a refresh token presented again within the grace of
// its rotation: the console lost the answer (network, closed tab, two tabs
// without a shared lock). While its family is alive the token counts as
// valid, and the family gets another pair; when the family was exchanged
// for an opaque session, that session gets a new token (pair). Otherwise,
// or when that session is gone, it is a replay.
func (s *Service) retryRefresh(ctx context.Context, tx *gorm.DB, t presentedRefresh) (refreshVerdict, *TokenPair, error) {
	g := store.G(ctx, tx)
	live, err := store.Exists(g, liveFamily(g, t.family))
	if err != nil {
		return 0, nil, err
	}
	if live {
		return refreshValid, nil, nil
	}
	if s.sessions == nil {
		return refreshReplayed, nil, nil
	}
	if err := activeUser(g, t.userID); err != nil {
		return 0, nil, err
	}
	user, err := s.loginUser(ctx, tx, t.userID)
	if err != nil {
		return 0, nil, err
	}
	token, err := s.sessions.Rotate(ctx, t.userID, t.family)
	if errors.Is(err, tokenstore.ErrNotFound) {
		return refreshReplayed, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return refreshValid, &TokenPair{AccessToken: token, ExpiresIn: int64(s.sessionIdle / time.Second), User: user}, nil
}

// endReplayedFamily revokes the refresh token family a replay was seen in,
// and the opaque session it may have been exchanged for (endOpaqueSession:
// Valkey down, a bump stands in). A row without a family (never written by
// this code) only revokes itself.
func (s *Service) endReplayedFamily(ctx context.Context, g *gorm.DB, t presentedRefresh) error {
	if t.family == "" {
		return revokeToken(g, t.id).Error
	}
	if err := revokeFamily(g, t.family).Error; err != nil {
		return err
	}
	return s.endOpaqueSession(ctx, g, t.userID, t.family)
}

// retireFamily spends a refresh token family exchanged for an opaque
// session of its id: its tokens are revoked and its login_sessions row
// goes. The session keeps how the family started, which ctx carries on.
func retireFamily(ctx context.Context, g *gorm.DB, userID int64, family string) (context.Context, error) {
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	if meta.method == "" {
		if err := familyMethod(g, family).Row().Scan(&meta.method); err != nil && !store.IsNoRows(err) {
			return ctx, err
		}
		ctx = withLoginMethod(ctx, meta.method)
	}
	if err := revokeFamily(g, family).Error; err != nil {
		return ctx, err
	}
	return ctx, deleteSessions(g, userID, oneSession(family)).Error
}

// activeUser checks that the undeleted user uid is active: a refresh of a
// deleted one fails like a bad token, of a disabled one with 403.
func activeUser(g *gorm.DB, uid int64) error {
	var status string
	err := liveUser(g, uid, "status").Row().Scan(&status)
	if store.IsNoRows(err) {
		return errBadRefresh
	}
	if err != nil {
		return err
	}
	if status != StatusActive {
		return errUserDisabled
	}
	return nil
}

// rotateRefresh marks the refresh token id used, keeping the first time
// it was.
func rotateRefresh(db *gorm.DB, id int64) *gorm.DB {
	return refreshTokens(db).Where("id = ?", id).Updates(map[string]any{
		"revoked_at":  gorm.Expr("coalesce(revoked_at, now())"),
		"replaced_at": gorm.Expr("coalesce(replaced_at, now())"),
	})
}

// Logout ends the session refreshToken belongs to, if it is a live token of
// userID. Without one, a caller signed in with an opaque session ends that
// session (it has no refresh token to name it by), whether or not it is
// still there; any other caller ends every session of the user.
func (s *Service) Logout(ctx context.Context, userID int64, refreshToken string) error {
	which := allSessions
	switch {
	case refreshToken != "":
		var family string
		err := liveFamilyOf(store.G(ctx, s.db.Gorm), tokenstore.Hash(refreshToken), userID).Row().Scan(&family)
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		which = oneSession(family)
	case core.OpaqueSession(ctx):
		which = oneSession(core.Session(ctx))
	}
	return s.changeSessions(ctx, userID, which, nil)
}

// liveFamilyOf reads the family of the live refresh token hash of userID.
func liveFamilyOf(db *gorm.DB, hash string, userID int64) *gorm.DB {
	return refreshByHash(db, hash).Select("family_id").Where("user_id = ? AND revoked_at IS NULL AND family_id <> ''", userID)
}

// ChangePassword changes the caller's password and ends every other session
// at once; keepFamily, the caller's own ("" through an access key: none),
// renews on its next 401. The route has verified the caller (step-up
// self.password.change: the password and a second factor).
func (s *Service) ChangePassword(ctx context.Context, userID int64, keepFamily, newPassword string) error {
	if f := validatePassword("new_password", newPassword); f != nil {
		return core.InvalidFields(*f)
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.cost)
	if err != nil {
		return err
	}
	return s.changeSessions(ctx, userID, otherSessions(keepFamily), func(tx *gorm.DB) error {
		res := setLivePassword(store.G(ctx, tx), userID, string(newHash))
		if res.Error == nil && res.RowsAffected == 0 {
			return core.ErrUnauthenticated
		}
		return res.Error
	})
}

// setLivePassword changes the password hash of the undeleted user userID.
func setLivePassword(db *gorm.DB, userID int64, hash string) *gorm.DB {
	return liveUsers(db).Where("id = ?", userID).
		Updates(map[string]any{"password_hash": hash, "updated_at": gorm.Expr("now()")})
}

// Me is the response of GET /me.
type Me struct {
	ID          int64    `json:"id"`
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Superuser   bool     `json:"superuser"`
	// AuthzVersion is the authz version Permissions were read at; a larger
	// X-Authz-Version on a later response means they may have changed.
	AuthzVersion int64 `json:"authz_version"`
}

// GetMe returns the caller's profile and effective permissions (superusers
// get every active permission).
func (s *Service) GetMe(ctx context.Context, userID int64) (*Me, error) {
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	set, err := s.authz.PermissionSet(ctx, userID)
	if err != nil {
		return nil, err
	}
	me := &Me{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, Roles: u.Roles, Superuser: set.Superuser, AuthzVersion: set.Version}
	if set.Superuser {
		if me.Permissions, err = s.authz.ActivePermissionKeys(ctx); err != nil {
			return nil, err
		}
	} else {
		// Canonical keys (account:read:own, account:update:group/12…).
		me.Permissions = set.KeyList()
	}
	sort.Strings(me.Permissions)
	return me, nil
}

package iam

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// TokenPair is the response of login and refresh. When the account has
// two-factor authentication, a correct password answers with only
// TwoFactorRequired and FlowToken; POST /auth/login/2fa gives the tokens.
type TokenPair struct {
	AccessToken       string `json:"access_token,omitempty"`
	RefreshToken      string `json:"refresh_token,omitempty"`
	ExpiresIn         int64  `json:"expires_in,omitempty"` // access token lifetime, seconds
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
		token, err := s.createFlow(ctx, flowLogin2FA, id, login2FAPayload{Email: limiterEmail(email), Password: hashToken(hash)})
		if err != nil {
			return nil, err
		}
		return &TokenPair{TwoFactorRequired: true, FlowToken: token, ExpiresIn: flowTTLSeconds}, nil
	}
	var pair *TokenPair
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		// The user row is the parent lock for every refresh-token family.
		// Recheck the password snapshot after acquiring it: a password change
		// may have committed while bcrypt was running above.
		g := store.G(ctx, tx)
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
		pair, err = s.issueTokens(ctx, tx, id, "")
		return err
	})
	return pair, err
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
	var raw []byte
	var userID int64
	err := liveFlow(store.G(ctx, s.db.Gorm), flowToken, flowLogin2FA, "payload, user_id").Row().Scan(&raw, &userID)
	if store.IsNoRows(err) {
		return nil, errFlowExpired
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if wait := s.limiter.check(ctx, payload.Email, ip); wait > 0 {
		return nil, rateLimitedError(ctx, wait)
	}
	var pair *TokenPair
	err = s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		var hash, status string
		if err := lockLiveUser(g, userID, "password_hash, status").Row().Scan(&hash, &status); err != nil {
			if store.IsNoRows(err) {
				return errBadCredentials
			}
			return err
		}
		if hashToken(hash) != payload.Password {
			return errFlowExpired
		}
		if status != StatusActive {
			return errUserDisabled
		}
		if err := s.checkSecondFactor(ctx, tx, userID, code); err != nil {
			return err
		}
		var used login2FAPayload
		if _, err := s.takeFlow(ctx, tx, flowToken, flowLogin2FA, userID, &used); err != nil {
			return err
		}
		if err := touchLastLogin(g, userID).Error; err != nil {
			return err
		}
		var err error
		pair, err = s.issueTokens(withLoginMethod(ctx, loginMethodTwoFactor), tx, userID, "")
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

// issueTokens issues an access token and a refresh token in familyID (a new
// family when empty, i.e. on login, which is audited for a superuser).
func (s *Service) issueTokens(ctx context.Context, tx *gorm.DB, userID int64, familyID string) (*TokenPair, error) {
	g := store.G(ctx, tx)
	var tokenVersion int
	if err := userTokenVersion(g, userID).Row().Scan(&tokenVersion); err != nil {
		return nil, err
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	newFamily := familyID == ""
	if newFamily {
		if familyID, err = newFamilyID(); err != nil {
			return nil, err
		}
		if err := s.auditSuperuserLogin(ctx, tx, userID); err != nil {
			return nil, err
		}
	}
	access, err := s.issueAccessToken(userID, tokenVersion, familyID)
	if err != nil {
		return nil, err
	}
	if err := s.touchLoginSession(ctx, tx, userID, familyID, newFamily); err != nil {
		return nil, err
	}
	if err := insertRefreshToken(g, userID, hashToken(refresh), time.Now().Add(s.cfg.RefreshTokenTTL), familyID).Error; err != nil {
		return nil, err
	}
	u, err := s.getUser(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	u.Balance = nil
	u.Segment = core.SegmentRef{}
	return &TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresIn: int64(s.cfg.AccessTokenTTL / time.Second), User: u}, nil
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
// (CONTRACTS §14.2) and fails with 401.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	if refreshToken == "" {
		return nil, errBadRefresh
	}
	var pair *TokenPair
	var replayUser int64
	var replayFamily string
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		var id, uid int64
		// Discover the parent without locking a child first. All issuers and
		// revokers serialize on the user, then read a fresh token snapshot.
		if err := refreshOwner(g, hashToken(refreshToken)).Row().Scan(&uid); err != nil {
			if store.IsNoRows(err) {
				return errBadRefresh
			}
			return err
		}
		if err := lockRefreshUser(ctx, tx, uid); err != nil {
			return err
		}
		var family string
		var revokedAt, replacedAt *time.Time
		var expiresAt time.Time
		err := lockRefreshToken(g, hashToken(refreshToken)).Row().Scan(&id, &uid, &family, &revokedAt, &replacedAt, &expiresAt)
		if store.IsNoRows(err) {
			return errBadRefresh
		}
		if err != nil {
			return err
		}
		verdict := classifyRefresh(revokedAt, replacedAt, expiresAt, time.Now())
		if verdict == refreshReplayed && family != "" && rotatedJustNow(revokedAt, replacedAt, time.Now()) {
			// The console lost the response of the rotation it just made
			// (network, closed tab, two tabs without a shared lock): while
			// the family is still alive, answer with another pair of the
			// same family instead of signing the user out.
			live, err := store.Exists(g, liveFamily(g, family))
			if err != nil {
				return err
			}
			if live {
				verdict = refreshValid
			}
		}
		switch verdict {
		case refreshReplayed:
			// Commit the family revocation; the 401 is returned after the tx.
			// A row without a family (never written by this code) only
			// revokes itself.
			if family == "" {
				family = "token:" + strconv.FormatInt(id, 10)
				err = revokeToken(g, id).Error
			} else {
				err = revokeFamily(g, family).Error
			}
			if err != nil {
				return err
			}
			replayUser, replayFamily = uid, family
			return nil
		case refreshExpired:
			return errBadRefresh
		}
		if err := rotateRefresh(g, id).Error; err != nil {
			return err
		}
		var status string
		err = liveUser(g, uid, "status").Row().Scan(&status)
		if store.IsNoRows(err) {
			return errBadRefresh
		}
		if err != nil {
			return err
		}
		if status != StatusActive {
			return errUserDisabled
		}
		pair, err = s.issueTokens(ctx, tx, uid, family)
		return err
	})
	if err != nil {
		return nil, err
	}
	if replayFamily != "" {
		slog.WarnContext(ctx, "iam: refresh token reuse detected, token family revoked",
			"user_id", replayUser, "family_id", replayFamily)
		return nil, errBadRefresh
	}
	return pair, nil
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
// userID; an empty token ends every session of the user.
func (s *Service) Logout(ctx context.Context, userID int64, refreshToken string) error {
	which := allSessions
	if refreshToken != "" {
		var family string
		err := liveFamilyOf(store.G(ctx, s.db.Gorm), hashToken(refreshToken), userID).Row().Scan(&family)
		if store.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		which = oneSession(family)
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

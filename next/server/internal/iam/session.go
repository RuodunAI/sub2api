package iam

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// How a login session started.
const (
	loginMethodPassword  = "password"
	loginMethodTwoFactor = "two_factor"
	loginMethodPasskey   = "passkey"
)

// LoginSession is one signed-in browser or device: a refresh token family with
// a live token.
type LoginSession struct {
	ID         string    `json:"id"`
	Method     string    `json:"method"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Current    bool      `json:"current"`
}

type sessionMeta struct {
	method, ip, userAgent string
}

type sessionMetaKey struct{}

func sessionContext(c *gin.Context, method string) context.Context {
	ua := c.Request.UserAgent()
	if utf8.RuneCountInString(ua) > 512 {
		ua = string([]rune(ua)[:512])
	}
	return context.WithValue(c.Request.Context(), sessionMetaKey{}, sessionMeta{method: method, ip: c.ClientIP(), userAgent: ua})
}

func withLoginMethod(ctx context.Context, method string) context.Context {
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	meta.method = method
	return context.WithValue(ctx, sessionMetaKey{}, meta)
}

// touchLoginSession records a new login session, or that an existing one was
// just used. A family from before these rows exists gets its row on the next
// refresh, with what is known then. A new login also clears the user's rows
// that have outlived any refresh token.
func (s *Service) touchLoginSession(ctx context.Context, tx pgx.Tx, userID int64, familyID string, isNew bool) error {
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	g := store.G(ctx, tx)
	if isNew {
		if err := deleteStaleSessions(g, userID, time.Now().Add(-s.cfg.RefreshTokenTTL)).Error; err != nil {
			return err
		}
	}
	return upsertLoginSession(g, familyID, userID, meta).Error
}

// deleteStaleSessions removes the login sessions of userID last used before
// before.
func deleteStaleSessions(db *gorm.DB, userID int64, before time.Time) *gorm.DB {
	return db.Where("user_id = ? AND last_used_at < ?", userID, before).Delete(&model.LoginSession{})
}

// upsertLoginSession records the login session familyID of userID, or that
// it was used again: an empty address or user agent keeps the one known.
func upsertLoginSession(db *gorm.DB, familyID string, userID int64, meta sessionMeta) *gorm.DB {
	return db.Exec(`INSERT INTO login_sessions (family_id, user_id, method, ip, user_agent) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (family_id) DO UPDATE SET last_used_at = now(),
			ip = CASE WHEN EXCLUDED.ip = '' THEN login_sessions.ip ELSE EXCLUDED.ip END,
			user_agent = CASE WHEN EXCLUDED.user_agent = '' THEN login_sessions.user_agent ELSE EXCLUDED.user_agent END`,
		familyID, userID, meta.method, meta.ip, meta.userAgent)
}

// auditSuperuserLogin records a new login of a superuser in tx, with how it
// started and from where, and logs a warning.
func (s *Service) auditSuperuserLogin(ctx context.Context, tx pgx.Tx, userID int64) error {
	su, err := s.authz.IsSuperuser(ctx, tx, userID)
	if err != nil || !su {
		return err
	}
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	if err := audit.Audit(audit.WithClientIP(ctx, meta.ip), tx, userID, "auth.superadmin_login", "user", strconv.FormatInt(userID, 10),
		map[string]any{"method": meta.method, "ip": meta.ip}); err != nil {
		return err
	}
	slog.WarnContext(ctx, "iam: super admin login", "user_id", userID, "method", meta.method, "ip", meta.ip)
	return nil
}

// Sessions lists the user's live login sessions, most recently used first.
// currentFamily marks the caller's own.
func (s *Service) Sessions(ctx context.Context, userID int64, currentFamily string) ([]LoginSession, error) {
	var rows []sessionRow
	if err := liveSessions(store.G(ctx, s.db.Pool), userID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LoginSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, LoginSession{ID: r.ID, Method: r.Method, IP: r.IP, UserAgent: r.UserAgent,
			CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Current: r.ID == currentFamily})
	}
	return out, nil
}

// sessionRow is a row of liveSessions.
type sessionRow struct {
	ID         string    `gorm:"column:id"`
	Method     string    `gorm:"column:method"`
	IP         string    `gorm:"column:ip"`
	UserAgent  string    `gorm:"column:user_agent"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	LastUsedAt time.Time `gorm:"column:last_used_at"`
}

// liveSessions are the refresh token families of userID with a live token,
// with what their login_sessions row knows, most recently used first.
func liveSessions(db *gorm.DB, userID int64) *gorm.DB {
	return db.Raw(`SELECT t.family_id AS id, COALESCE(l.method, '') AS method, COALESCE(l.ip, '') AS ip, COALESCE(l.user_agent, '') AS user_agent,
			COALESCE(l.created_at, t.created_at) AS created_at, COALESCE(l.last_used_at, t.created_at) AS last_used_at
		FROM (SELECT family_id, min(created_at) AS created_at FROM refresh_tokens
			WHERE user_id = ? AND revoked_at IS NULL AND expires_at > now() AND family_id <> ''
			GROUP BY family_id) t
		LEFT JOIN login_sessions l ON l.family_id = t.family_id
		ORDER BY 6 DESC`, userID)
}

// RevokeSession ends one of the user's login sessions at once: its refresh
// token stops and its access token is rejected; the other sessions renew on
// their next 401.
func (s *Service) RevokeSession(ctx context.Context, userID int64, familyID string) error {
	return s.changeSessions(ctx, userID, oneSession(familyID), func(tx pgx.Tx) error {
		g := store.G(ctx, tx)
		live, err := store.Exists(g, userFamily(g, userID, familyID))
		if err != nil {
			return err
		}
		if !live {
			return core.ErrNotFound
		}
		return audit.Audit(ctx, tx, userID, "user.session.revoke", "user", strconv.FormatInt(userID, 10), nil)
	})
}

// userFamily are the unrevoked tokens of the refresh token family of userID.
func userFamily(db *gorm.DB, userID int64, familyID string) *gorm.DB {
	return refreshTokens(db).Where("user_id = ? AND family_id = ? AND revoked_at IS NULL", userID, familyID)
}

// RevokeOtherSessions ends every login session of the user but keepFamily.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID int64, keepFamily string) error {
	return s.changeSessions(ctx, userID, otherSessions(keepFamily), func(tx pgx.Tx) error {
		return audit.Audit(ctx, tx, userID, "user.session.revoke_others", "user", strconv.FormatInt(userID, 10), nil)
	})
}

// sessionsToEnd picks the refresh-token families invalidateSessions revokes:
// only that one family, or every family but keep (empty fields select all).
type sessionsToEnd struct{ only, keep string }

var allSessions = sessionsToEnd{}

func otherSessions(keep string) sessionsToEnd { return sessionsToEnd{keep: keep} }

func oneSession(family string) sessionsToEnd { return sessionsToEnd{only: family} }

// invalidateSessions stops every access token of userID at once
// (token_version+1, CONTRACTS §43.2) and revokes the refresh tokens and
// login sessions `which` selects; a session whose refresh token survives
// renews on its next 401. Callers lock the user row first and call
// sessionsChanged after commit.
func invalidateSessions(ctx context.Context, tx pgx.Tx, userID int64, which sessionsToEnd) (revoked int64, err error) {
	g := store.G(ctx, tx)
	if err := bumpTokenVersion(g, userID).Error; err != nil {
		return 0, err
	}
	res := revokeSessions(g, userID, which)
	if res.Error != nil {
		return 0, res.Error
	}
	if err := deleteSessions(g, userID, which).Error; err != nil {
		return 0, err
	}
	return res.RowsAffected, nil
}

// revokeSessions revokes the live refresh tokens of the families of userID
// that `which` selects.
func revokeSessions(db *gorm.DB, userID int64, which sessionsToEnd) *gorm.DB {
	return revokeRefresh(which.of(refreshTokens(db).Where("user_id = ? AND revoked_at IS NULL", userID)))
}

// deleteSessions removes the login sessions of userID that `which` selects.
func deleteSessions(db *gorm.DB, userID int64, which sessionsToEnd) *gorm.DB {
	return which.of(db.Where("user_id = ?", userID)).Delete(&model.LoginSession{})
}

// of narrows db to the families `which` selects. Each value is bound twice:
// an empty one selects every family.
func (which sessionsToEnd) of(db *gorm.DB) *gorm.DB {
	return db.Where("(? = '' OR family_id = ?) AND (? = '' OR family_id IS DISTINCT FROM ?)", which.only, which.only, which.keep, which.keep)
}

// bumpTokenVersion stops every access token of userID.
func bumpTokenVersion(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.User{}).Where("id = ?", userID).Update("token_version", gorm.Expr("token_version + 1"))
}

// changeSessions runs fn (optional) and invalidateSessions in one transaction
// holding the user row lock, then announces the change. It takes no authz
// lock; a caller that needs one opens its own transaction in the authz ->
// user lock order and calls invalidateSessions and sessionsChanged itself.
func (s *Service) changeSessions(ctx context.Context, userID int64, which sessionsToEnd, fn func(tx pgx.Tx) error) error {
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshUser(ctx, tx, userID); err != nil {
			return err
		}
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		_, err := invalidateSessions(ctx, tx, userID, which)
		return err
	})
	if err != nil {
		return err
	}
	s.sessionsChanged(ctx, userID)
	return nil
}

// sessionsChanged drops the cached status of uids on this node and asks the
// other nodes to do the same (iam:user:changed). A lost message is covered by
// the reconnect reset and statusCacheTTL.
func (s *Service) sessionsChanged(ctx context.Context, uids ...int64) {
	for _, uid := range uids {
		s.forgetStatus(uid)
		if s.bus == nil {
			continue
		}
		if err := s.bus.Publish(ctx, core.ChannelUserSessionsChanged, []byte(strconv.FormatInt(uid, 10))); err != nil {
			slog.WarnContext(ctx, "iam: publish iam:user:changed failed", "user_id", uid, "err", err)
		}
	}
}

// currentFamily is the caller's login session (core.Session).
func (s *Service) currentFamily(c *gin.Context) string {
	return core.Session(c.Request.Context())
}

// UpdateProfile changes what the user may change about themselves.
func (s *Service) UpdateProfile(ctx context.Context, userID int64, displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if utf8.RuneCountInString(displayName) > 100 {
		return core.InvalidFields(core.FieldError{Field: "display_name", Code: "too_long", Message: "at most 100 characters"})
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		if err := setLiveProfile(store.G(ctx, tx), userID, displayName).Error; err != nil {
			return err
		}
		return audit.Audit(ctx, tx, userID, "user.profile.update", "user", strconv.FormatInt(userID, 10), map[string]any{"display_name": displayName})
	})
}

// setLiveProfile changes the display name of the undeleted user userID.
func setLiveProfile(db *gorm.DB, userID int64, displayName string) *gorm.DB {
	return liveUsers(db).Where("id = ?", userID).
		Updates(map[string]any{"display_name": displayName, "updated_at": gorm.Expr("now()")})
}

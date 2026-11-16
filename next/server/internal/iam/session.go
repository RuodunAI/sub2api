package iam

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// How a login session started.
const (
	loginMethodPassword  = "password"
	loginMethodTwoFactor = "two_factor"
	loginMethodPasskey   = "passkey"
)

// LoginSession is one signed-in browser or device: an opaque session, or a
// refresh token family with a live token.
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
func (s *Service) touchLoginSession(ctx context.Context, tx *gorm.DB, userID int64, familyID string, isNew bool) error {
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
func (s *Service) auditSuperuserLogin(ctx context.Context, tx *gorm.DB, userID int64) error {
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

// Sessions lists the user's live login sessions, the opaque ones and the
// refresh token families, most recently used first. currentFamily marks the
// caller's own.
func (s *Service) Sessions(ctx context.Context, userID int64, currentFamily string) ([]LoginSession, error) {
	var rows []sessionRow
	if err := liveSessions(store.G(ctx, s.db.Gorm), userID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	var opaque []tokenstore.Session
	if s.sessions != nil {
		var err error
		opaque, err = s.sessions.List(ctx, userID)
		// Issuing JWTs, Valkey only holds what is left of an earlier switch
		// to s2s: the list goes on without it.
		if tokenstore.IsUnavailable(err) && !s.issuesOpaque(ctx) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
	}
	// A session a bump ended may still be in Valkey (its cleanup failed,
	// the server CLI): it is not listed, as it is not accepted.
	_, version, err := s.isActiveWithVersion(ctx, userID, 0)
	if err != nil {
		return nil, err
	}
	out := make([]LoginSession, 0, len(rows)+len(opaque))
	seen := make(map[string]bool, len(opaque))
	for _, o := range opaque {
		if o.TokenVersion < version {
			continue
		}
		seen[o.ID] = true
		out = append(out, LoginSession{ID: o.ID, Method: o.Method, IP: o.IP, UserAgent: o.UserAgent,
			CreatedAt: o.CreatedAt, LastUsedAt: o.LastSeen, Current: o.ID == currentFamily})
	}
	for _, r := range rows {
		if !seen[r.ID] { // a family being exchanged for its opaque session
			out = append(out, LoginSession{ID: r.ID, Method: r.Method, IP: r.IP, UserAgent: r.UserAgent,
				CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, Current: r.ID == currentFamily})
		}
	}
	slices.SortStableFunc(out, func(a, b LoginSession) int { return b.LastUsedAt.Compare(a.LastUsedAt) })
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

// RevokeSession ends one of the user's login sessions at once: an opaque
// one is gone, or its refresh token stops and its access token is
// rejected; the other JWT sessions renew on their next 401.
func (s *Service) RevokeSession(ctx context.Context, userID int64, familyID string) error {
	return s.changeSessions(ctx, userID, oneSession(familyID), func(tx *gorm.DB) error {
		live, err := s.hasSession(ctx, store.G(ctx, tx), userID, familyID)
		if err != nil {
			return err
		}
		if !live {
			return core.ErrNotFound
		}
		return audit.Audit(ctx, tx, userID, "user.session.revoke", "user", strconv.FormatInt(userID, 10), nil)
	})
}

// hasSession reports whether id is a live login session of userID: a
// refresh token family with a live token, or an opaque session no bump
// ended. One a bump ended may still be in Valkey (its cleanup failed, the
// server CLI); it is not there, as the sessions list and authentication
// tell.
func (s *Service) hasSession(ctx context.Context, g *gorm.DB, userID int64, id string) (bool, error) {
	live, err := store.Exists(g, userFamily(g, userID, id))
	if err != nil || live || s.sessions == nil {
		return live, err
	}
	var version int
	if err := userTokenVersion(g, userID).Row().Scan(&version); err != nil {
		return false, err
	}
	return s.sessions.Has(ctx, userID, id, version)
}

// userFamily are the unrevoked tokens of the refresh token family of userID.
func userFamily(db *gorm.DB, userID int64, familyID string) *gorm.DB {
	return refreshTokens(db).Where("user_id = ? AND family_id = ? AND revoked_at IS NULL", userID, familyID)
}

// RevokeOtherSessions ends every login session of the user but keepFamily.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID int64, keepFamily string) error {
	return s.changeSessions(ctx, userID, otherSessions(keepFamily), func(tx *gorm.DB) error {
		return audit.Audit(ctx, tx, userID, "user.session.revoke_others", "user", strconv.FormatInt(userID, 10), nil)
	})
}

// sessionsToEnd picks the login sessions a change ends: only that one, or
// every one but keep (empty fields select all).
type sessionsToEnd struct{ only, keep string }

var allSessions = sessionsToEnd{}

func otherSessions(keep string) sessionsToEnd { return sessionsToEnd{keep: keep} }

func oneSession(family string) sessionsToEnd { return sessionsToEnd{only: family} }

// Ending sessions stops their access JWTs through a token_version bump
// (CONTRACTS §43.2), and the opaque sessions through the stamp each one
// carries (verifyOpaque rejects one below the row). A bump that restamps
// the opaque sessions it keeps steps by 1. A bump that does not reach
// Valkey steps by stamplessStep: a restamp whose transaction rolled back
// leaves a stamp one ahead of the row (never more: each restamp is to the
// row plus 1, once per transaction, keepingBump), and every opaque session
// must be passed. Since a restamp only moves the sessions stamped at the
// row or above (tokenstore.Bump), a session any bump ended stays ended.
const stamplessStep = 2

// endAllSessions ends every session of userID in tx, with PostgreSQL alone:
// the refresh tokens, the login_sessions rows, and a stampless bump, which
// rejects every opaque session. Valkey being down thus does not stop an
// operator from disabling a stolen account. The caller holds the user row
// lock and calls sessionsEnded after the commit, which removes the opaque
// sessions from Valkey.
func (s *Service) endAllSessions(ctx context.Context, tx *gorm.DB, userID int64) error {
	g := store.G(ctx, tx)
	if err := endFamilies(g, userID, allSessions); err != nil {
		return err
	}
	return bumpTokenVersion(g, userID, stamplessStep).Error
}

// endSomeSessions ends the sessions `which` selects and keeps the others,
// in tx: an opaque session no JWT of whose id may still be valid alone,
// with no bump (endOpaqueSession); otherwise through a bump that keeps the
// other opaque sessions (keepingBump). The caller calls sessionsChanged
// after the commit, and calls this once per transaction (keepingBump).
func (s *Service) endSomeSessions(ctx context.Context, tx *gorm.DB, userID int64, which sessionsToEnd) error {
	g := store.G(ctx, tx)
	if err := endFamilies(g, userID, which); err != nil {
		return err
	}
	if which.only != "" {
		// A JWT is issued with a refresh token of its family: none issued
		// within the access token TTL (and a minute of clock skew) means
		// none is valid.
		jwt, err := store.Exists(g, familyIssuedSince(g, userID, which.only, time.Now().Add(-s.cfg.AccessTokenTTL-time.Minute)))
		if err != nil {
			return err
		}
		if !jwt {
			return s.endOpaqueSession(ctx, g, userID, which.only)
		}
	}
	return s.keepingBump(ctx, g, userID, which)
}

// endOpaqueSession ends the opaque session id of userID, and nothing else.
// When Valkey cannot be reached, a stampless bump ends it with every other
// opaque session (the JWT sessions renew on their next 401): the change
// goes through, erring on the safe side.
func (s *Service) endOpaqueSession(ctx context.Context, g *gorm.DB, userID int64, id string) error {
	return s.reviseOrBump(ctx, g, userID, tokenstore.Selection{Only: id}, tokenstore.Bump{}, stamplessStep)
}

// keepingBump bumps token_version by 1, which stops every JWT (a JWT
// session kept renews on its next 401), and in the same step ends the
// opaque sessions `which` selects and stamps the others with the new
// version: they have no refresh token to renew with. When Valkey cannot be
// reached the bump goes on to a stampless one, which ends the opaque
// sessions kept too (a JWT caller signing out does not need Valkey).
//
// The restamp is safe on two conditions. The version it starts from is the
// latest committed one, read here under the row lock, so no login slips a
// session in with the old version and no restamp starts below a bump that
// committed meanwhile. And a transaction restamps once at most: a stamp is
// then never more than one ahead of the row, even when the transaction
// rolls back, which stamplessStep relies on; a second restamp in the same
// transaction would stamp two ahead. endSomeSessions is its only caller.
func (s *Service) keepingBump(ctx context.Context, g *gorm.DB, userID int64, which sessionsToEnd) error {
	var from int
	if err := userTokenVersion(g, userID).Clauses(store.ForUpdate).Row().Scan(&from); err != nil {
		return err
	}
	if err := bumpTokenVersion(g, userID, 1).Error; err != nil {
		return err
	}
	return s.reviseOrBump(ctx, g, userID, tokenstore.Selection{Only: which.only, Keep: which.keep},
		tokenstore.Bump{From: from, To: from + 1}, stamplessStep-1)
}

// reviseOrBump runs a tokenstore Revoke. When Valkey cannot be reached (or
// there is no store: the server CLI) it bumps token_version by step
// instead, which with the caller's own bump makes a stampless one.
func (s *Service) reviseOrBump(ctx context.Context, g *gorm.DB, userID int64, sel tokenstore.Selection, b tokenstore.Bump, step int) error {
	if s.sessions != nil {
		err := s.sessions.Revoke(ctx, userID, sel, b)
		if !tokenstore.IsUnavailable(err) {
			return err
		}
		slog.WarnContext(ctx, "iam: opaque sessions not revised, a token_version bump ends them", "user_id", userID, "err", err)
	}
	return bumpTokenVersion(g, userID, step).Error
}

// familyIssuedSince are the refresh tokens of the family of userID issued
// after since, revoked or not.
func familyIssuedSince(db *gorm.DB, userID int64, familyID string, since time.Time) *gorm.DB {
	return refreshTokens(db).Where("user_id = ? AND family_id = ? AND created_at > ?", userID, familyID, since)
}

// endFamilies revokes the live refresh tokens and removes the login_sessions
// rows of the families of userID that `which` selects.
func endFamilies(db *gorm.DB, userID int64, which sessionsToEnd) error {
	if err := revokeSessions(db, userID, which).Error; err != nil {
		return err
	}
	return deleteSessions(db, userID, which).Error
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

// bumpTokenVersion stops every access token of userID, stepping the
// version by step.
func bumpTokenVersion(db *gorm.DB, userID int64, step int) *gorm.DB {
	return db.Model(&model.User{}).Where("id = ?", userID).Update("token_version", gorm.Expr("token_version + ?", step))
}

// changeSessions runs fn (optional) and ends the sessions `which` selects
// in one transaction holding the user row lock, then announces the change.
// It takes no authz lock; a caller that needs one opens its own transaction
// in the authz -> user lock order and calls endAllSessions and
// sessionsEnded itself.
func (s *Service) changeSessions(ctx context.Context, userID int64, which sessionsToEnd, fn func(tx *gorm.DB) error) error {
	all := which == allSessions
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		if err := lockRefreshUser(ctx, tx, userID); err != nil {
			return err
		}
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		if all {
			return s.endAllSessions(ctx, tx, userID)
		}
		return s.endSomeSessions(ctx, tx, userID, which)
	})
	if err != nil {
		return err
	}
	if all {
		s.sessionsEnded(ctx, userID)
	} else {
		s.sessionsChanged(ctx, userID)
	}
	return nil
}

// sessionsEnded follows the commit of endAllSessions of uids:
// sessionsChanged, then the opaque sessions the bump ended leave Valkey.
// Only those stamped below the version committed go: a login since the
// commit stays. The bump already rejects them, so this is cleanup: a
// failure is logged, and the sessions go when next presented, or idle out.
func (s *Service) sessionsEnded(ctx context.Context, uids ...int64) {
	s.sessionsChanged(ctx, uids...)
	if s.sessions == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, uid := range uids {
		var tv int
		err := userTokenVersion(store.G(ctx, s.db.Gorm), uid).Row().Scan(&tv)
		if err == nil {
			err = s.sessions.Prune(ctx, uid, tv)
		}
		if err != nil {
			slog.ErrorContext(ctx, "iam: removing ended opaque sessions failed, token_version rejects them", "user_id", uid, "err", err)
		}
	}
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
	return s.db.Tx(ctx, func(tx *gorm.DB) error {
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

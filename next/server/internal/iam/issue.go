package iam

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// What a login issues (session.issue, CONTRACTS §100). Every node accepts
// both JWTs and opaque sessions; the switch only picks what is handed out,
// so it is flipped once every node runs a version that accepts s2s_, with no
// second release. With s2s, a refresh token presented to /auth/refresh is
// exchanged for an opaque session, and new auth flows are written to Valkey
// only; with jwt they still go to auth_flows, where older nodes read them.
const (
	IssueJWT    = "jwt"
	IssueOpaque = "s2s"
)

// IssueEnv pins session.issue on this process.
const IssueEnv = "SUP2API_SESSION_ISSUE"

// issueSwitch is session.issue: the settings row "session" holding
// {"issue": ...}, flipped the way authz.engine is (authz.Switch).
var issueSwitch = authz.Switch{
	Key: "session", Field: "issue", Env: IssueEnv,
	Values: []string{IssueJWT, IssueOpaque}, Default: IssueJWT, Action: "session.issue.set",
	Denied: core.LocalizedText{"en": "only the super admin can switch how logins are issued", "zh": "只有超级管理员可以切换登录凭证的签发方式"},
}

// issueCacheTTL bounds how long a node that missed the config:changed of a
// switch keeps issuing the other kind. Logins are rare: reading the row
// again this often costs nothing, and no poll task is needed.
const issueCacheTTL = 30 * time.Second

// issueCached is the mode read at at.
type issueCached struct {
	mode string
	at   time.Time
}

// issueModeState holds what issueMode answers from: the pinned mode, or
// the cached setting (nil: read it again).
type issueModeState struct {
	pinned    string
	hasPinned bool
	cached    atomic.Pointer[issueCached]
}

// issueMode is what this node issues now. A failed read keeps the last
// mode known (jwt before any): the login that asked fails on the same
// database anyway.
func (s *Service) issueMode(ctx context.Context) string {
	if s.issue.hasPinned {
		return s.issue.pinned
	}
	c := s.issue.cached.Load()
	if c != nil && time.Since(c.at) < issueCacheTTL {
		return c.mode
	}
	v, err := issueSwitch.Stored(ctx, s.db.Gorm)
	if err != nil {
		slog.WarnContext(ctx, "iam: reading session.issue failed", "err", err)
		if c != nil {
			return c.mode
		}
		return IssueJWT
	}
	mode := issueSwitch.Parse(v)
	s.issue.cached.Store(&issueCached{mode: mode, at: time.Now()})
	return mode
}

// issuesOpaque reports whether logins issue opaque sessions here: the
// switch says so and there is a session store (the server CLI has none).
func (s *Service) issuesOpaque(ctx context.Context) bool {
	return s.sessions != nil && s.issueMode(ctx) == IssueOpaque
}

// issueChanged follows a change of session.issue made on any node.
func (s *Service) issueChanged(payload []byte) {
	if issueSwitch.Changed(payload) {
		s.issue.cached.Store(nil)
	}
}

// IssueStatus is what GET /settings/session answers.
type IssueStatus struct {
	Issue     string `json:"issue"`     // as stored, "" when never set
	Effective string `json:"effective"` // what this node issues
	Source    string `json:"source"`    // setting | env | default
	// IdleTTLSeconds and MaxTTLSeconds bound an opaque session.
	IdleTTLSeconds int64 `json:"idle_ttl_seconds"`
	MaxTTLSeconds  int64 `json:"max_ttl_seconds"`
}

// IssueStatus reports session.issue as stored and as this node applies it.
func (s *Service) IssueStatus(ctx context.Context) (IssueStatus, error) {
	v, err := issueSwitch.Stored(ctx, s.db.Gorm)
	if err != nil {
		return IssueStatus{}, err
	}
	s.issue.cached.Store(nil)
	return IssueStatus{Issue: v, Effective: s.issueMode(ctx), Source: issueSwitch.Source(s.issue.hasPinned, v),
		IdleTTLSeconds: int64(s.cfg.SessionIdleTTL / time.Second), MaxTTLSeconds: int64(s.cfg.SessionMaxTTL / time.Second)}, nil
}

// SetIssue stores session.issue and tells every node (authz.SetSwitch:
// super admin only, audited session.issue.set). IssueEnv still wins on the
// nodes that set it.
func (s *Service) SetIssue(ctx context.Context, actorID int64, mode string) (IssueStatus, error) {
	if err := s.authz.SetSwitch(ctx, issueSwitch, actorID, mode); err != nil {
		return IssueStatus{}, err
	}
	return s.IssueStatus(ctx)
}

func (s *Service) handleGetIssue(c *gin.Context) {
	st, err := s.IssueStatus(c.Request.Context())
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, st)
}

func (s *Service) handlePutIssue(c *gin.Context) {
	var in struct {
		Issue string `json:"issue"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	st, err := s.SetIssue(c.Request.Context(), actor(c), in.Issue)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, st)
}

// registerIssueRoutes mounts GET/PUT /settings/session, as /settings/authz:
// switching takes a step-up proof (session.issue) besides settings:manage,
// and only the super admin passes SetIssue.
func (s *Service) registerIssueRoutes(r *httpapi.Router) {
	r.Perm(http.MethodGet, "/settings/session", "settings:read", s.handleGetIssue)
	r.StepUp(core.StepUpSessionIssue).Perm(http.MethodPut, "/settings/session", "settings:manage", s.handlePutIssue)
}

// ------------------------------------------------------------ opaque sessions

// issueOpaque stores an opaque session of userID under the login session
// familyID and returns its token. tokenVersion was read under the user's
// lock, which the caller holds, so a revocation cannot slip in between.
// A family that is not new is a refresh token being exchanged: its tokens
// are spent and its login_sessions row goes, the session lives on in Valkey
// under the same id (the step-up proofs bound to it stay valid).
func (s *Service) issueOpaque(ctx context.Context, g *gorm.DB, userID int64, tokenVersion int, familyID string, isNew bool) (*TokenPair, error) {
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	if !isNew {
		if meta.method == "" {
			if err := familyMethod(g, familyID).Row().Scan(&meta.method); err != nil && !store.IsNoRows(err) {
				return nil, err
			}
		}
		if err := revokeFamily(g, familyID).Error; err != nil {
			return nil, err
		}
		if err := deleteSessions(g, userID, oneSession(familyID)).Error; err != nil {
			return nil, err
		}
	}
	token, err := s.sessions.Create(ctx, tokenstore.Session{ID: familyID, UserID: userID, Method: meta.method,
		IP: meta.ip, UserAgent: meta.userAgent, TokenVersion: tokenVersion})
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: token, ExpiresIn: int64(s.sessionIdle / time.Second)}, nil
}

// familyMethod reads how the login session familyID started.
func familyMethod(db *gorm.DB, familyID string) *gorm.DB {
	return db.Model(&model.LoginSession{}).Select("method").Where("family_id = ?", familyID)
}

// verifyOpaque is VerifySession for an s2s_ token. Valkey down fails with
// 503 (tokenstore.ErrUnavailable); there is no fallback. A session from
// before a token_version bump that did not reach Valkey (the server CLI) is
// dropped on sight.
func (s *Service) verifyOpaque(ctx context.Context, token string) (int64, string, error) {
	if s.sessions == nil {
		return 0, "", core.ErrUnauthenticated
	}
	sess, err := s.sessions.Lookup(ctx, token)
	if errors.Is(err, tokenstore.ErrNotFound) {
		return 0, "", core.ErrUnauthenticated.WithMessage("session has ended")
	}
	if err != nil {
		return 0, "", err
	}
	active, version, err := s.isActiveWithVersion(ctx, sess.UserID, sess.TokenVersion)
	if err != nil {
		return 0, "", err
	}
	// Survivors of a bump are stamped with the new version before it
	// commits, so a session may be ahead of the row: only an older one is
	// stale.
	if !active || sess.TokenVersion < version {
		if err := s.sessions.Revoke(ctx, sess.UserID, tokenstore.Selection{Only: sess.ID}, 0); err != nil {
			return 0, "", err
		}
		return 0, "", core.ErrUnauthenticated.WithMessage("session has been invalidated")
	}
	if err := s.sessions.Touch(ctx, token, sess); err != nil {
		return 0, "", err
	}
	return sess.UserID, sess.ID, nil
}

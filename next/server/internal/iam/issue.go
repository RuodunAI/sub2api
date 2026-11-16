package iam

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/settingswitch"
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
const IssueEnv = "SUB2API_SESSION_ISSUE"

// issueSwitch is session.issue: the settings row "session" holding
// {"issue": ...}, flipped the way authz.engine is.
var issueSwitch = settingswitch.Switch{
	Key: "session", Field: "issue", Env: IssueEnv,
	Values: []string{IssueJWT, IssueOpaque}, Default: IssueJWT, Action: "session.issue.set", Log: "iam",
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
// the cached setting (nil: none read yet).
type issueModeState struct {
	pinned    string
	hasPinned bool
	cached    atomic.Pointer[issueCached]
}

// expire makes issueMode read the setting again, keeping the mode known
// for a read that fails.
func (st *issueModeState) expire() {
	if c := st.cached.Load(); c != nil {
		st.cached.Store(&issueCached{mode: c.mode})
	}
}

// issueMode is what this node issues now. A failed read, or a value that
// is not a string, keeps the last mode known (jwt before any).
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
		s.issue.expire()
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
	// Behind are the live nodes that do not accept opaque sessions yet
	// (core.CapabilityOpaqueSessions): s2s is refused while there are any.
	// null when the node registry could not be read.
	Behind []BehindNode `json:"behind"`
}

// BehindNode is a live node of an older build.
type BehindNode struct {
	NodeID      string `json:"node_id"`
	BootID      string `json:"boot_id"`
	HostVersion string `json:"host_version"`
}

// LiveNodes lists the nodes online now (cluster.Registry).
type LiveNodes interface {
	LiveNodes(ctx context.Context) ([]core.NodeStatus, error)
}

// IssueStatus reports session.issue as stored and as this node applies it.
func (s *Service) IssueStatus(ctx context.Context) (IssueStatus, error) {
	v, err := issueSwitch.Stored(ctx, s.db.Gorm)
	if err != nil {
		return IssueStatus{}, err
	}
	behind, err := s.nodesBehind(ctx)
	if err != nil { // unknown (null): the switch back to jwt reports without it
		slog.WarnContext(ctx, "iam: listing the live nodes failed", "err", err)
	}
	s.issue.expire()
	effective := IssueJWT
	if s.issuesOpaque(ctx) {
		effective = IssueOpaque
	}
	return IssueStatus{Issue: v, Effective: effective, Source: issueSwitch.Source(s.issue.hasPinned, v),
		IdleTTLSeconds: int64(s.cfg.SessionIdleTTL / time.Second), MaxTTLSeconds: int64(s.cfg.SessionMaxTTL / time.Second),
		Behind: behind}, nil
}

// nodesBehind lists the live nodes without core.CapabilityOpaqueSessions:
// they would answer an s2s_ token with 401 and miss the flows in Valkey.
// Without a registry (the server CLI, tests) none is known.
func (s *Service) nodesBehind(ctx context.Context) ([]BehindNode, error) {
	out := []BehindNode{}
	if s.nodes == nil {
		return out, nil
	}
	nodes, err := s.nodes.LiveNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if !slices.Contains(n.Capabilities, core.CapabilityOpaqueSessions) {
			out = append(out, BehindNode{NodeID: n.NodeID, BootID: n.BootID, HostVersion: n.HostVersion})
		}
	}
	return out, nil
}

// SetIssue stores session.issue and tells every node (settingswitch:
// super admin only, audited session.issue.set). Switching to s2s waits for
// every live node to accept opaque sessions: 409 lists the ones behind.
// Switching back to jwt is always possible. IssueEnv still wins on the
// nodes that set it.
func (s *Service) SetIssue(ctx context.Context, actorID int64, mode string) (IssueStatus, error) {
	if mode == IssueOpaque {
		behind, err := s.nodesBehind(ctx)
		if err != nil {
			return IssueStatus{}, err
		}
		if len(behind) > 0 {
			return IssueStatus{}, core.ErrConflict.WithMessage(t(ctx,
				"some nodes do not accept opaque sessions yet: upgrade them first",
				"部分节点还不支持不透明会话，请先升级这些节点")).WithDetails(map[string]any{"nodes": behind})
		}
	}
	if err := issueSwitch.Set(ctx, s.db, s.bus, s.authz.IsSuperuser, actorID, mode); err != nil {
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
func (s *Service) issueOpaque(ctx context.Context, userID int64, tokenVersion int, familyID string) (*TokenPair, error) {
	meta, _ := ctx.Value(sessionMetaKey{}).(sessionMeta)
	token, err := s.sessions.Create(ctx, tokenstore.Session{ID: familyID, UserID: userID, Method: meta.method,
		IP: meta.ip, UserAgent: meta.userAgent, TokenVersion: tokenVersion})
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: token, ExpiresIn: int64(s.sessionIdle / time.Second), opaqueID: familyID}, nil
}

// familyMethod reads how the login session familyID started.
func familyMethod(db *gorm.DB, familyID string) *gorm.DB {
	return db.Model(&model.LoginSession{}).Select("method").Where("family_id = ?", familyID)
}

// verifyOpaque is VerifySession for an s2s_ token. Valkey down fails with
// 503 (tokenstore.ErrUnavailable); there is no fallback. A session from
// before a token_version bump that did not reach Valkey (ending every
// session, the server CLI) is dropped on sight; a token a rotation left in
// its grace drops only itself, never the session's current token.
//
// The user's status is read through the cache whatever the session's
// stamp: a stamp ahead of the row (a survivor of a bump not committed yet,
// or rolled back) is accepted, and a stale cache can only be behind a newer
// session, which it accepts too.
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
	active, version, err := s.isActiveWithVersion(ctx, sess.UserID, 0)
	if err != nil {
		return 0, "", err
	}
	if !active || sess.TokenVersion < version {
		if err := s.sessions.Drop(ctx, token, sess); err != nil {
			return 0, "", err
		}
		return 0, "", core.ErrUnauthenticated.WithMessage("session has been invalidated")
	}
	if err := s.sessions.Touch(ctx, token, sess); err != nil {
		return 0, "", err
	}
	return sess.UserID, sess.ID, nil
}

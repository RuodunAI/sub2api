package authz

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/settingswitch"
)

// EngineMode says which resolver answers the effective permission sets
// (authz.engine, CONTRACTS §99): the legacy loader, the legacy loader with
// Casbin compared beside it, or Casbin.
type EngineMode string

const (
	EngineLegacy EngineMode = "legacy"
	EngineShadow EngineMode = "shadow"
	EngineCasbin EngineMode = "casbin"
)

// engineState is the mode a node resolves with and its generation, one
// more at each switch. The cached sets (Service.users) are all of the
// current generation: a switch clears them under Service.mu, and a load
// started under another generation does not store its set (a load still
// running when the operator switched back to legacy must not leave Casbin's
// answer behind).
type engineState struct {
	mode EngineMode
	gen  uint64
}

// engineRefreshTimeout bounds a read of authz.engine off a request (a
// config:changed broadcast): as the version poll's timeout.
const engineRefreshTimeout = 10 * time.Second

// EngineEnv overrides the authz.engine setting on this process, for tests
// and emergencies (a node that cannot read the setting, or must not follow
// it).
const EngineEnv = "SUB2API_AUTHZ_ENGINE"

// engineSwitch is authz.engine: the settings row "authz" holding
// {"engine": ...}, whose change config:changed announces as
// {"key": "settings.authz"}.
var engineSwitch = settingswitch.Switch{
	Key: "authz", Field: "engine", Env: EngineEnv,
	Values:  []string{string(EngineLegacy), string(EngineShadow), string(EngineCasbin)},
	Default: string(EngineShadow), Action: "authz.engine.set", Log: "authz",
	Denied: core.LocalizedText{"en": "only the super admin can switch the authorization engine", "zh": "只有超级管理员可以切换权限引擎"},
}

// ParseEngine reads an engine name. Anything else, empty included, is
// shadow: an unreadable value keeps the legacy answers and keeps comparing.
func ParseEngine(s string) EngineMode { return EngineMode(engineSwitch.Parse(s)) }

// EngineSettings is the "authz" settings row.
type EngineSettings struct {
	Engine string `json:"engine"`
}

// EngineStatus is what GET /settings/authz answers: the stored setting, the
// engine this node uses and why, and this node's shadow counters.
type EngineStatus struct {
	Engine    string      `json:"engine"`
	Effective EngineMode  `json:"effective"`
	Source    string      `json:"source"` // setting | env | default
	Shadow    ShadowStats `json:"shadow"`
}

// envEngine is the engine EngineEnv sets, if it is set.
func envEngine() (EngineMode, bool) {
	e, ok := engineSwitch.Pinned()
	return EngineMode(e), ok
}

// EngineMode is the engine this node resolves permission sets with; a
// service without a database is always legacy.
func (s *Service) EngineMode() EngineMode { return s.engine.Load().mode }

// setEngine switches this node to m. Cached sets were resolved by the
// engine being left, so a switch drops them, and moves the generation on
// so that no load started before it stores its set (engineState).
func (s *Service) setEngine(m EngineMode) {
	s.mu.Lock()
	cur := s.engine.Load()
	if cur.mode == m {
		s.mu.Unlock()
		return
	}
	s.engine.Store(&engineState{mode: m, gen: cur.gen + 1})
	clear(s.users)
	s.mu.Unlock()
	slog.Info("authz: engine switched", "engine", m)
	s.prewarm(s.Version())
}

// refreshEngine reads authz.engine again (start, config:changed, the
// version poll). EngineEnv pins the engine; a failed read keeps the current
// one.
func (s *Service) refreshEngine(ctx context.Context) {
	if s.engineEnv || s.db == nil {
		return
	}
	v, err := s.engineSetting(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.WarnContext(ctx, "authz: reading authz.engine failed", "err", err)
		}
		return
	}
	s.setEngine(ParseEngine(v.Engine))
}

// engineSetting reads the "authz" settings row; a missing row is the
// default (an empty engine, shadow).
func (s *Service) engineSetting(ctx context.Context) (EngineSettings, error) {
	e, err := engineSwitch.Stored(ctx, s.db.Gorm)
	return EngineSettings{Engine: e}, err
}

// onConfigChanged follows a change of authz.engine made on another node.
func (s *Service) onConfigChanged(payload []byte) {
	if engineSwitch.Changed(payload) {
		// Off the bus goroutine, which must not block on the database, and
		// bounded: every settings change is broadcast, and a stuck pool
		// must not pile the reads up.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), engineRefreshTimeout)
			defer cancel()
			s.refreshEngine(ctx)
		}()
	}
}

// EngineStatus reports authz.engine as stored and as this node applies it.
func (s *Service) EngineStatus(ctx context.Context) (EngineStatus, error) {
	v, err := s.engineSetting(ctx)
	if err != nil {
		return EngineStatus{}, err
	}
	return EngineStatus{Engine: v.Engine, Effective: s.EngineMode(), Source: engineSwitch.Source(s.engineEnv, v.Engine), Shadow: s.shadow.stats()}, nil
}

// SetEngine stores authz.engine and tells every node. Only the super admin
// may: the engine decides who answers every permission check of the site
// (the route also wants settings:manage and a step-up proof). The change
// is audited (authz.engine.set) in the transaction that stores it.
// EngineEnv still wins on the nodes that set it.
func (s *Service) SetEngine(ctx context.Context, actorID int64, e string) (EngineStatus, error) {
	if err := engineSwitch.Set(ctx, s.db, s.bus, s.IsSuperuser, actorID, e); err != nil {
		return EngineStatus{}, err
	}
	if !s.engineEnv {
		s.setEngine(ParseEngine(e))
	}
	return s.EngineStatus(ctx)
}

func (s *Service) handleGetEngine(c *gin.Context) {
	st, err := s.EngineStatus(c.Request.Context())
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, st)
}

func (s *Service) handlePutEngine(c *gin.Context) {
	var in EngineSettings
	if !httpapi.BindJSON(c, &in) {
		return
	}
	ctx := c.Request.Context()
	uid, _ := core.UserID(ctx)
	st, err := s.SetEngine(ctx, uid, in.Engine)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, st)
}

// registerEngineRoutes mounts GET/PUT /settings/authz. Switching takes a
// step-up proof (authz.engine, §80.2) besides settings:manage, and only the
// super admin passes SetEngine.
func (s *Service) registerEngineRoutes(r *httpapi.Router) {
	r.Perm(http.MethodGet, "/settings/authz", "settings:read", s.handleGetEngine)
	r.StepUp(core.StepUpAuthzEngine).Perm(http.MethodPut, "/settings/authz", "settings:manage", s.handlePutEngine)
}

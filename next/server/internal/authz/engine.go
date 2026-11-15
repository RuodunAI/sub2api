package authz

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// Engine says which resolver answers the effective permission sets
// (authz.engine, CONTRACTS §99): the legacy loader, the legacy loader with
// Casbin compared beside it, or Casbin.
type Engine string

const (
	EngineLegacy Engine = "legacy"
	EngineShadow Engine = "shadow"
	EngineCasbin Engine = "casbin"
)

// EngineEnv overrides the authz.engine setting on this process, for tests
// and emergencies (a node that cannot read the setting, or must not follow
// it).
const EngineEnv = "SUP2API_AUTHZ_ENGINE"

// engineSettingKey is the settings row holding {"engine": ...}; the
// config:changed payload of its change is {"key": "settings.authz"}.
const engineSettingKey = "authz"

var engineChangedPayload = []byte(`{"key":"settings.` + engineSettingKey + `"}`)

// ParseEngine reads an engine name. Anything else, empty included, is
// shadow: an unreadable value keeps the legacy answers and keeps comparing.
func ParseEngine(s string) Engine {
	switch e := Engine(s); e {
	case EngineLegacy, EngineShadow, EngineCasbin:
		return e
	}
	return EngineShadow
}

func validEngine(s string) bool { return ParseEngine(s) == Engine(s) }

// EngineSettings is the "authz" settings row.
type EngineSettings struct {
	Engine string `json:"engine"`
}

// EngineStatus is what GET /settings/authz answers: the stored setting, the
// engine this node uses and why, and this node's shadow counters.
type EngineStatus struct {
	Engine    string      `json:"engine"`
	Effective Engine      `json:"effective"`
	Source    string      `json:"source"` // setting | env | default
	Shadow    ShadowStats `json:"shadow"`
}

// envEngine is the engine EngineEnv sets, if it is set.
func envEngine() (Engine, bool) {
	raw, ok := os.LookupEnv(EngineEnv)
	if !ok || raw == "" {
		return "", false
	}
	if !validEngine(raw) {
		slog.Warn("authz: invalid "+EngineEnv+", using shadow", "value", raw)
	}
	return ParseEngine(raw), true
}

// Engine is the engine this node resolves permission sets with. Without a
// Casbin resolver (a service with no database) it is always legacy.
func (s *Service) Engine() Engine {
	if s.casbin == nil {
		return EngineLegacy
	}
	return *s.engine.Load()
}

// setEngine switches this node to e. Cached sets were resolved by the
// engine being left, so a switch drops them: switching back to legacy must
// not keep serving what Casbin answered.
func (s *Service) setEngine(e Engine) {
	if old := s.engine.Swap(&e); *old == e {
		return
	}
	s.mu.Lock()
	clear(s.users)
	s.mu.Unlock()
	slog.Info("authz: engine switched", "engine", e)
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
	var v EngineSettings
	raw, err := store.SettingValue(store.G(ctx, s.db.Gorm), engineSettingKey)
	if store.IsNoRows(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	return v, json.Unmarshal(raw, &v)
}

// onConfigChanged follows a change of authz.engine made on another node.
func (s *Service) onConfigChanged(payload []byte) {
	var event struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(payload, &event) == nil && event.Key == "settings."+engineSettingKey {
		// Off the bus goroutine, which must not block on the database.
		go s.refreshEngine(context.Background())
	}
}

// EngineStatus reports authz.engine as stored and as this node applies it.
func (s *Service) EngineStatus(ctx context.Context) (EngineStatus, error) {
	v, err := s.engineSetting(ctx)
	if err != nil {
		return EngineStatus{}, err
	}
	src := "default"
	switch {
	case s.engineEnv:
		src = "env"
	case v.Engine != "":
		src = "setting"
	}
	return EngineStatus{Engine: v.Engine, Effective: s.Engine(), Source: src, Shadow: s.shadow.stats()}, nil
}

// SetEngine stores authz.engine and tells every node. EngineEnv still wins
// on the nodes that set it.
func (s *Service) SetEngine(ctx context.Context, actorID int64, e string) (EngineStatus, error) {
	if !validEngine(e) {
		return EngineStatus{}, core.InvalidFields(core.FieldError{Field: "engine", Code: "invalid",
			Message: t(ctx, "must be legacy, shadow or casbin", "必须是 legacy、shadow 或 casbin")})
	}
	var by *int64
	if actorID > 0 {
		by = &actorID
	}
	v := EngineSettings{}
	if err := store.PatchSettingJSON(ctx, s.db, engineSettingKey, by, EngineSettings{Engine: e}, &v); err != nil {
		return EngineStatus{}, err
	}
	if !s.engineEnv {
		s.setEngine(ParseEngine(v.Engine))
	}
	if s.bus != nil {
		if err := s.bus.Publish(context.WithoutCancel(ctx), core.ChannelConfigChanged, engineChangedPayload); err != nil {
			slog.WarnContext(ctx, "authz: publish authz.engine change failed", "err", err)
		}
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

// registerEngineRoutes mounts GET/PUT /settings/authz.
func (s *Service) registerEngineRoutes(r *httpapi.Router) {
	r.Perm(http.MethodGet, "/settings/authz", "settings:read", s.handleGetEngine)
	r.Perm(http.MethodPut, "/settings/authz", "settings:manage", s.handlePutEngine)
}

package authz

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
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
const EngineEnv = "SUP2API_AUTHZ_ENGINE"

// engineSettingKey is the settings row holding {"engine": ...}; the
// config:changed payload of its change is {"key": "settings.authz"}.
const engineSettingKey = "authz"

var engineChangedPayload = []byte(`{"key":"settings.` + engineSettingKey + `"}`)

// ParseEngine reads an engine name. Anything else, empty included, is
// shadow: an unreadable value keeps the legacy answers and keeps comparing.
func ParseEngine(s string) EngineMode {
	switch e := EngineMode(s); e {
	case EngineLegacy, EngineShadow, EngineCasbin:
		return e
	}
	return EngineShadow
}

func validEngine(s string) bool { return ParseEngine(s) == EngineMode(s) }

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
	raw, ok := os.LookupEnv(EngineEnv)
	if !ok || raw == "" {
		return "", false
	}
	if !validEngine(raw) {
		slog.Warn("authz: invalid "+EngineEnv+", using shadow", "value", raw)
	}
	return ParseEngine(raw), true
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
	src := "default"
	switch {
	case s.engineEnv:
		src = "env"
	case v.Engine != "":
		src = "setting"
	}
	return EngineStatus{Engine: v.Engine, Effective: s.EngineMode(), Source: src, Shadow: s.shadow.stats()}, nil
}

// SetEngine stores authz.engine and tells every node. Only the super admin
// may: the engine decides who answers every permission check of the site
// (the route also wants settings:manage and a step-up proof). The change
// is audited (authz.engine.set) in the transaction that stores it.
// EngineEnv still wins on the nodes that set it.
func (s *Service) SetEngine(ctx context.Context, actorID int64, e string) (EngineStatus, error) {
	if !validEngine(e) {
		return EngineStatus{}, core.InvalidFields(core.FieldError{Field: "engine", Code: "invalid",
			Message: t(ctx, "must be legacy, shadow or casbin", "必须是 legacy、shadow 或 casbin")})
	}
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		q := store.G(ctx, tx)
		su, err := s.IsSuperuser(ctx, q, actorID)
		if err != nil {
			return err
		}
		if !su {
			return core.ErrPermissionDenied.WithMessage(t(ctx,
				"only the super admin can switch the authorization engine", "只有超级管理员可以切换权限引擎"))
		}
		var from EngineSettings
		if _, err := store.UpdateSettingJSONTx(ctx, tx, engineSettingKey, &actorID, func(raw json.RawMessage) (json.RawMessage, error) {
			return withEngine(raw, e, &from)
		}); err != nil {
			return err
		}
		return s.audit(ctx, q, actorID, "authz.engine.set", "setting", engineSettingKey,
			map[string]any{"from": from.Engine, "to": e, "request_id": core.RequestID(ctx)})
	})
	if err != nil {
		return EngineStatus{}, err
	}
	if !s.engineEnv {
		s.setEngine(ParseEngine(e))
	}
	if s.bus != nil {
		if err := s.bus.Publish(context.WithoutCancel(ctx), core.ChannelConfigChanged, engineChangedPayload); err != nil {
			slog.WarnContext(ctx, "authz: publish authz.engine change failed", "err", err)
		}
	}
	return s.EngineStatus(ctx)
}

// withEngine is the settings document raw with its engine set to e (other
// fields kept); from receives the engine it held.
func withEngine(raw json.RawMessage, e string, from *EngineSettings) (json.RawMessage, error) {
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if old, ok := doc["engine"]; ok {
		_ = json.Unmarshal(old, &from.Engine) // a non-string is audited as ""
	}
	v, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	doc["engine"] = v
	return json.Marshal(doc)
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

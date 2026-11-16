package gateway

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

const settingsKeyScheduling = "scheduling"

// Values of SchedulingSettings.Metric (CONTRACTS §89).
const (
	balanceMetricRPM         = "rpm"
	balanceMetricConcurrency = "concurrency"
	balanceMetricTPM         = "tpm"
	balanceMetricSessions    = "sessions"
)

var balanceMetrics = []string{balanceMetricRPM, balanceMetricConcurrency, balanceMetricTPM, balanceMetricSessions}

// SchedulingSettings is the "scheduling" row of the settings table: how the
// core balances new sessions inside one (priority, route grade) tier
// (CONTRACTS §89). The traffic balance is always on.
type SchedulingSettings struct {
	Metric string `json:"metric"`
	// LimitLoad lowers an account's weight as Metric nears its limit.
	LimitLoad bool `json:"limit_load"`
	// ModelLimits balances by the request's model: the traffic balance
	// compares the model's own usage on each account, and LimitLoad also
	// counts the model list holding it when the account has one.
	ModelLimits bool `json:"model_limits"`
}

func defaultSchedulingSettings() SchedulingSettings {
	return SchedulingSettings{Metric: balanceMetricSessions}
}

// normalized turns a stored row into the settings the core runs: an unknown
// metric becomes the default.
func (s SchedulingSettings) normalized() SchedulingSettings {
	if !slices.Contains(balanceMetrics, s.Metric) {
		s.Metric = defaultSchedulingSettings().Metric
	}
	return s
}

func loadSchedulingSettings(ctx context.Context, q *gorm.DB) (SchedulingSettings, error) {
	v := defaultSchedulingSettings()
	err := loadSettingsRow(ctx, q, settingsKeyScheduling, &v)
	return v.normalized(), err
}

// validate checks the metric; messages follow the request locale.
func (s SchedulingSettings) validate(ctx context.Context) []core.FieldError {
	msg := "invalid value"
	if core.Locale(ctx) == "zh" {
		msg = "取值无效"
	}
	var fe []core.FieldError
	if !slices.Contains(balanceMetrics, s.Metric) {
		fe = append(fe, core.FieldError{Field: "metric", Code: "invalid", Message: msg})
	}
	return fe
}

func (g *Gateway) getSchedulingHandler(c *gin.Context) {
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	v, err := loadSchedulingSettings(c.Request.Context(), db.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, v)
}

// putSchedulingHandler replaces the row (absent fields take their defaults),
// then invalidates the local settings cache and broadcasts config:changed.
func (g *Gateway) putSchedulingHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	in := defaultSchedulingSettings()
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if fe := in.validate(ctx); len(fe) > 0 {
		httpapi.Fail(c, core.InvalidFields(fe...))
		return
	}
	uid, _ := core.UserID(ctx)
	if err := db.Tx(ctx, func(tx *gorm.DB) error {
		_, err := store.UpdateSettingJSONTx(ctx, tx, settingsKeyScheduling, nullID(uid), func(json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(in)
		})
		return err
	}); err != nil {
		httpapi.Fail(c, err)
		return
	}
	g.changed(ctx, "settings.scheduling")
	httpapi.OK(c, in)
}

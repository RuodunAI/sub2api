package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/platforms"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// RegisterRoutes mounts the sticky-session console API (CONTRACTS §5.6) and
// the gateway settings (CONTRACTS §14.4).
func (g *Gateway) RegisterRoutes(r *httpapi.Router) {
	r.Perm("GET", "/settings/failure-policy", "settings:read", g.getFailurePolicy)
	r.Perm("PUT", "/settings/failure-policy", "settings:manage", g.putFailurePolicy)
	r.Perm("POST", "/settings/failure-policy/preview", "settings:read", g.previewFailurePolicy)
	r.Perm("GET", "/settings/failure-policy/history", "settings:read", g.failurePolicyHistory)
	r.Perm("GET", "/settings/gateway", "settings:read", g.getGatewaySettingsHandler)
	r.Perm("PUT", "/settings/gateway", "settings:manage", g.putGatewaySettingsHandler)
	r.Perm("GET", "/settings/auto-disable", "settings:read", g.getAutoDisableHandler)
	r.Perm("PUT", "/settings/auto-disable", "settings:manage", g.putAutoDisableHandler)
	r.Perm("GET", "/settings/scheduling", "settings:read", g.getSchedulingHandler)
	r.Perm("PUT", "/settings/scheduling", "settings:manage", g.putSchedulingHandler)
	r.Perm("GET", "/sticky-rules", "sticky:read", g.listRulesHandler)
	r.Perm("POST", "/sticky-rules", "sticky:manage", g.createRuleHandler)
	r.Perm("GET", "/sticky-rules/stats", "sticky:read", g.statsHandler)
	r.Perm("PATCH", "/sticky-rules/:id", "sticky:manage", g.updateRuleHandler)
	r.Perm("DELETE", "/sticky-rules/:id", "sticky:manage", g.deleteRuleHandler)
	r.Perm("POST", "/sticky-rules/:id/flush", "sticky:manage", g.flushRuleHandler)
	r.Perm("POST", "/sticky-rules/:id/reset", "sticky:manage", g.resetRuleHandler)
	r.Perm("GET", "/settings/sticky", "sticky:read", g.getStickySettingsHandler)
	r.Perm("PUT", "/settings/sticky", "sticky:manage", g.putStickySettingsHandler)
}

// ---------------------------------------------------------------- DTOs

type ruleMatchDTO struct {
	Protocols         []string `json:"protocols"`
	Models            []string `json:"models"`
	UserAgentContains []string `json:"user_agent_contains"`
}

type keySourceDTO struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
	Name string `json:"name,omitempty"`
}

type ruleDTO struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Source      string         `json:"source"`
	Enabled     bool           `json:"enabled"`
	Priority    int            `json:"priority"`
	Match       ruleMatchDTO   `json:"match"`
	KeySources  []keySourceDTO `json:"key_sources"`
	ValueRegex  string         `json:"value_regex"`
	TTLSeconds  int            `json:"ttl_seconds"`
	KeyIncludes []string       `json:"key_includes"`
	OnFailure   string         `json:"on_failure"`
	PassHeaders []string       `json:"pass_headers"`
	Customized  bool           `json:"customized"`
	UpdatedBy   *int64         `json:"updated_by"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toDTO(r *stickyRule) ruleDTO {
	d := ruleDTO{
		ID: r.ID, Name: r.Name, Source: r.Source, Enabled: r.Enabled, Priority: r.Priority,
		Match: ruleMatchDTO{Protocols: nonNil(r.Match.Protocols), Models: nonNil(r.Match.Models),
			UserAgentContains: nonNil(r.Match.UserAgentContains)},
		KeySources: []keySourceDTO{}, ValueRegex: r.ValueRegex, TTLSeconds: r.TTLSeconds,
		KeyIncludes: nonNil(r.KeyIncludes), OnFailure: r.OnFailure, PassHeaders: nonNil(r.PassHeaders), Customized: r.Customized, UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt.UTC(),
	}
	for _, s := range r.KeySources {
		d.KeySources = append(d.KeySources, keySourceDTO{Type: s.Type, Path: s.Path, Name: s.Name})
	}
	return d
}

// ruleInput is the POST/PATCH body; nil fields are left unchanged on PATCH.
type ruleInput struct {
	Name        *string         `json:"name"`
	Enabled     *bool           `json:"enabled"`
	Priority    *int            `json:"priority"`
	Match       *ruleMatchDTO   `json:"match"`
	KeySources  *[]keySourceDTO `json:"key_sources"`
	ValueRegex  *string         `json:"value_regex"`
	TTLSeconds  *int            `json:"ttl_seconds"`
	KeyIncludes *[]string       `json:"key_includes"`
	OnFailure   *string         `json:"on_failure"`
	PassHeaders *[]string       `json:"pass_headers"`
}

func (in *ruleInput) applyTo(r *stickyRule) {
	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		r.Priority = *in.Priority
	}
	if in.Match != nil {
		r.Match = manifest.StickyMatch{Protocols: in.Match.Protocols, Models: in.Match.Models,
			UserAgentContains: in.Match.UserAgentContains}
	}
	if in.KeySources != nil {
		r.KeySources = r.KeySources[:0]
		for _, s := range *in.KeySources {
			r.KeySources = append(r.KeySources, manifest.StickyKeySource{Type: s.Type, Path: s.Path, Name: s.Name})
		}
	}
	if in.ValueRegex != nil {
		r.ValueRegex = *in.ValueRegex
	}
	if in.TTLSeconds != nil {
		r.TTLSeconds = *in.TTLSeconds
	}
	if in.KeyIncludes != nil {
		r.KeyIncludes = *in.KeyIncludes
	}
	if in.OnFailure != nil {
		r.OnFailure = *in.OnFailure
	}
	if in.PassHeaders != nil {
		r.PassHeaders = *in.PassHeaders
	}
}

var ruleNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,99}$`)

// knownProtocol reports whether a match.protocols entry names a protocol the
// current generation declares: protocols come from the endpoint declarations
// of the built-in platforms and of the platforms enabled plugins declare, and
// the registry indexes them (core.Generation.PlatformForProtocol). Patterns
// ("anthropic.*", "*") are accepted when they match at least one of them,
// matchList being how rules compare protocols at request time.
//
// It returns nil when the registry is unavailable (unit tests, a node whose
// generation is not loaded yet): the caller then skips the check instead of
// rejecting the rule.
func (g *Gateway) knownProtocol() func(string) bool {
	if g.d.Registry == nil {
		return nil
	}
	gen := g.d.Registry.Current()
	if gen == nil {
		return nil
	}
	return func(protocol string) bool {
		if _, ok := gen.PlatformForProtocol(protocol); ok {
			return true
		}
		if !strings.ContainsAny(protocol, "*?") {
			return false
		}
		for _, p := range gen.Platforms() {
			if matchList([]string{protocol}, p.Platform.Protocols()...) {
				return true
			}
		}
		return false
	}
}

// keeping extends a protocol check with the values a stored rule already has:
// an administrator editing a rule whose platform went away (plugin disabled or
// uninstalled) can still save it, while a new or mistyped protocol is still
// rejected. It returns nil when known is nil (check skipped).
func keeping(known func(string) bool, stored []string) func(string) bool {
	if known == nil {
		return nil
	}
	kept := make(map[string]bool, len(stored))
	for _, p := range stored {
		kept[p] = true
	}
	return func(protocol string) bool { return kept[protocol] || known(protocol) }
}

// validateRule checks an admin rule; messages follow the request locale.
// knownProtocol validates match.protocols; nil skips that check, for a request
// that does not carry a match (a PATCH of the switches only) or when the
// plugin registry is unavailable.
func validateRule(ctx context.Context, r *stickyRule, knownProtocol func(string) bool) []core.FieldError {
	loc := core.Locale(ctx)
	t := func(en, zh string) string {
		if loc == "zh" {
			return zh
		}
		return en
	}
	var fe []core.FieldError
	add := func(field, code, en, zh string) {
		fe = append(fe, core.FieldError{Field: field, Code: code, Message: t(en, zh)})
	}
	if !ruleNameRe.MatchString(r.Name) || r.Name == "stats" {
		add("name", "invalid", "letters, digits, '_', '.', '-' (max 100); \"stats\" is reserved",
			"只能包含字母、数字、_ . -（最多 100 个字符），且不能为 stats")
	}
	if len(r.KeySources) == 0 {
		add("key_sources", "required", "at least one key source is required", "至少需要一个取值来源")
	}
	for i, s := range r.KeySources {
		field := "key_sources[" + itoa(int64(i)) + "]"
		switch s.Type {
		case "body":
			if strings.TrimSpace(s.Path) == "" {
				add(field+".path", "required", "path is required for body sources", "body 来源必须填写 path")
			}
		case "header":
			if strings.TrimSpace(s.Name) == "" {
				add(field+".name", "required", "name is required for header sources", "header 来源必须填写 name")
			}
		case "api_key", "user":
		default:
			// The session value comes from the request; no plugin computes it.
			add(field+".type", "invalid", "type must be body, header, api_key or user",
				"type 只能是 body、header、api_key 或 user")
		}
	}
	if r.ValueRegex != "" {
		if _, err := regexp.Compile(r.ValueRegex); err != nil {
			add("value_regex", "invalid", "invalid regular expression: "+err.Error(), "正则表达式无效："+err.Error())
		}
	}
	if r.TTLSeconds < 0 || r.TTLSeconds > 30*24*3600 {
		add("ttl_seconds", "out_of_range", "must be between 0 (default) and 2592000", "取值范围为 0（使用默认值）到 2592000")
	}
	for _, d := range r.KeyIncludes {
		if d != "group" && d != "model" && d != "rule" {
			add("key_includes", "invalid", "allowed values: group, model, rule", "只能包含 group、model、rule")
			break
		}
	}
	if r.OnFailure != onFailureFailover && r.OnFailure != onFailureStick {
		add("on_failure", "invalid", "must be failover or stick", "只能是 failover 或 stick")
	}
	passHeaders, passErrs := core.NormalizeHeaderPassthrough(ctx, "pass_headers", r.PassHeaders)
	r.PassHeaders = passHeaders
	fe = append(fe, passErrs...)
	// An empty list means "any protocol"; a typo in a listed protocol would
	// silently never match, so it is rejected here. All offenders go into one
	// field error: a console keyed by field name shows a single message.
	if knownProtocol != nil {
		var bad []string
		for _, p := range r.Match.Protocols {
			if !knownProtocol(p) {
				bad = append(bad, "\""+p+"\"")
			}
		}
		if len(bad) > 0 {
			list := strings.Join(bad, ", ")
			add("match.protocols", "invalid",
				"unknown protocol(s) "+list+"; protocols come from the endpoint declarations of the installed platforms",
				"协议 "+list+" 不存在；可用协议来自已注册平台的端点声明")
		}
	}
	return fe
}

// ---------------------------------------------------------------- handlers

func (g *Gateway) db() (*store.DB, error) {
	if g.d.DB == nil {
		return nil, core.ErrUnavailable
	}
	return g.d.DB, nil
}

func (g *Gateway) listRulesHandler(c *gin.Context) {
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	rules, err := listRules(c.Request.Context(), db.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	out := make([]ruleDTO, 0, len(rules))
	for _, r := range rules {
		out = append(out, toDTO(r))
	}
	httpapi.OK(c, out)
}

func getRule(ctx context.Context, q *gorm.DB, id int64) (*stickyRule, error) {
	var row model.StickyRule
	err := store.G(ctx, q).Where("id = ?", id).Take(&row).Error
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return ruleOf(&row), nil
}

// ruleOf is the rule of a sticky_rules row, as scanRule reads it.
func ruleOf(m *model.StickyRule) *stickyRule {
	r := &stickyRule{ID: m.ID, Name: m.Name, Source: m.Source, Enabled: core.ValueOr(m.Enabled, false),
		Priority: core.ValueOr(m.Priority, 0), ValueRegex: m.ValueRegex, TTLSeconds: core.ValueOr(m.TTLSeconds, 0),
		OnFailure: core.ValueOr(m.OnFailure, ""), PassHeaders: m.PassHeaders, Customized: m.Customized,
		UpdatedBy: m.UpdatedBy, UpdatedAt: m.UpdatedAt}
	r.decodeDocuments(m.Match, m.KeySources, m.KeyIncludes)
	return r
}

func nullID(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (g *Gateway) createRuleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	var in ruleInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	r := &stickyRule{Source: sourceAdmin, Enabled: true, Priority: 100, OnFailure: onFailureFailover,
		KeyIncludes: append([]string(nil), defaultKeyIncludes...)}
	in.applyTo(r)
	if fe := validateRule(ctx, r, g.knownProtocol()); len(fe) > 0 {
		httpapi.Fail(c, core.InvalidFields(fe...))
		return
	}
	uid, _ := core.UserID(ctx)
	// updated_at is left to its default, now().
	row := &model.StickyRule{Name: r.Name, Source: sourceAdmin, Enabled: &r.Enabled, Priority: &r.Priority,
		Match: mustJSON(r.Match), KeySources: mustJSON(r.KeySources), ValueRegex: r.ValueRegex,
		TTLSeconds: &r.TTLSeconds, KeyIncludes: mustJSON(nonNil(r.KeyIncludes)), OnFailure: &r.OnFailure,
		UpdatedBy: nullID(uid), PassHeaders: nonNil(r.PassHeaders)}
	err = store.G(ctx, db.Gorm).Create(row).Error
	if store.IsUniqueViolation(err, "") {
		httpapi.Fail(c, core.ErrConflict.WithMessage("an admin sticky rule with this name already exists"))
		return
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	g.changed(ctx, "sticky_rules")
	created, err := getRule(ctx, db.Gorm, row.ID)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.Created(c, toDTO(created))
}

func (g *Gateway) updateRuleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	var in ruleInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	r, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if r.Source != sourceAdmin && in.Name != nil && strings.TrimSpace(*in.Name) != r.Name {
		// A built-in rule is known by its name: the start-up sync and the
		// admin rules that shadow it find it by name.
		msg := "built-in rules cannot be renamed; create an admin rule to use another name"
		if core.Locale(ctx) == "zh" {
			msg = "内置规则不能改名；如需其它名称，请新建管理员规则"
		}
		httpapi.Fail(c, core.InvalidFields(core.FieldError{Field: "name", Code: "invalid", Message: msg}))
		return
	}
	stored := append([]string(nil), r.Match.Protocols...)
	before := ruleDefinition(r)
	in.applyTo(r)
	// An edited definition of a built-in rule is the administrator's from
	// now on: upgrades keep it until it is restored to the default
	// (resetRuleHandler). The console submits the whole rule, so only a
	// definition that actually changed counts.
	changed := ruleDefinition(r) != before
	customized := r.Customized || r.Source != sourceAdmin && changed
	// Protocols are only checked when the request carries a match, and
	// the ones the rule already had are kept: the console submits the
	// whole rule, so changing the TTL or a switch of a rule whose
	// platform went away (plugin disabled or uninstalled) must not be
	// rejected. A protocol added by this request is still checked.
	var known func(string) bool
	if in.Match != nil {
		known = keeping(g.knownProtocol(), stored)
	}
	if r.Source == sourceAdmin || changed {
		if fe := validateRule(ctx, r, known); len(fe) > 0 {
			httpapi.Fail(c, core.InvalidFields(fe...))
			return
		}
	}
	uid, _ := core.UserID(ctx)
	err = store.G(ctx, db.Gorm).Model(&model.StickyRule{}).Where("id = ?", id).Updates(map[string]any{
		"name": r.Name, "enabled": r.Enabled, "priority": r.Priority, "match": mustJSON(r.Match),
		"key_sources": mustJSON(r.KeySources), "value_regex": r.ValueRegex, "ttl_seconds": r.TTLSeconds,
		"key_includes": mustJSON(nonNil(r.KeyIncludes)), "on_failure": r.OnFailure, "updated_by": nullID(uid),
		"customized": customized, "pass_headers": store.Strings(nonNil(r.PassHeaders)), "updated_at": gorm.Expr("now()"),
	}).Error
	if store.IsUniqueViolation(err, "") {
		httpapi.Fail(c, core.ErrConflict.WithMessage("an admin sticky rule with this name already exists"))
		return
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	g.changed(ctx, "sticky_rules")
	updated, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, toDTO(updated))
}

func (g *Gateway) deleteRuleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	r, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if r.Source != sourceAdmin {
		msg := "built-in rules cannot be deleted; disable them instead"
		if core.Locale(ctx) == "zh" {
			msg = "内置规则不能删除，可以停用"
		}
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage(msg))
		return
	}
	if err := store.G(ctx, db.Gorm).Where("id = ?", id).Delete(&model.StickyRule{}).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	// Bindings under this name may be shared with a built-in rule of the
	// same name, so they are left to expire; only the counters go.
	if g.d.Redis != nil && !g.ruleNameInUse(ctx, db, r.Name) {
		_ = g.d.Redis.Del(ctx, stickyStatsKey(r.Name)).Err()
	}
	g.changed(ctx, "sticky_rules")
	httpapi.NoContent(c)
}

func (g *Gateway) ruleNameInUse(ctx context.Context, db *store.DB, name string) bool {
	// An error reads as not in use: the caller then drops the rule's stats.
	gdb := store.G(ctx, db.Gorm)
	found, _ := gormpg.Exists(gdb, gdb.Model(&model.StickyRule{}).Where("name = ?", name))
	return found
}

// flushRuleHandler deletes every binding of the rule.
func (g *Gateway) flushRuleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	r, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	// Without "rule" in key_includes the bindings are keyed sticky:_:… and
	// shared with every other such rule; they cannot be told apart.
	if !r.includes("rule") {
		msg := "this rule's bindings are shared with other rules (key_includes has no \"rule\") and cannot be flushed on their own; they expire with their TTL"
		if core.Locale(ctx) == "zh" {
			msg = "该规则的 key_includes 不含 rule，绑定与其他同类规则共用，无法单独清除；绑定会在 TTL 到期后失效"
		}
		httpapi.Fail(c, core.ErrConflict.WithMessage(msg))
		return
	}
	n, err := g.flushBindings(ctx, r.Name)
	if err != nil {
		httpapi.Fail(c, core.ErrUnavailable.WithCause(err))
		return
	}
	httpapi.OK(c, gin.H{"deleted": n})
}

func (g *Gateway) flushBindings(ctx context.Context, rule string) (int64, error) {
	if g.d.Redis == nil {
		return 0, nil
	}
	var total int64
	err := scanStickyKeys(ctx, g.d.Redis, stickyBindingPattern(rule), func(keys []string) error {
		n, err := g.d.Redis.Del(ctx, keys...).Result()
		total += n
		return err
	})
	return total, err
}

type ruleStat struct {
	RuleID  int64  `json:"rule_id"`
	Rule    string `json:"rule"`
	Source  string `json:"source"`
	Hits    int64  `json:"hits"`
	Misses  int64  `json:"misses"`
	Rebinds int64  `json:"rebinds"`
}

func (g *Gateway) statsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	rules, err := listRules(ctx, db.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	out := make([]ruleStat, 0, len(rules))
	for _, r := range rules {
		s := ruleStat{RuleID: r.ID, Rule: r.Name, Source: r.Source}
		if g.d.Redis != nil {
			m, err := g.d.Redis.HGetAll(ctx, stickyStatsKey(r.Name)).Result()
			if err != nil {
				httpapi.Fail(c, core.ErrUnavailable.WithCause(err))
				return
			}
			s.Hits, s.Misses, s.Rebinds = parseInt(m["hits"]), parseInt(m["misses"]), parseInt(m["rebinds"])
		}
		out = append(out, s)
	}
	httpapi.OK(c, out)
}

func (g *Gateway) getStickySettingsHandler(c *gin.Context) {
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	v, err := loadStickySettings(c.Request.Context(), db.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	p, err := loadFailurePolicy(c.Request.Context(), db.Gorm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, struct {
		StickySettings
		FailurePolicyManaged bool `json:"failure_policy_managed"`
	}{v, p != nil})
}

func (g *Gateway) putStickySettingsHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	cur := defaultStickySettings()
	var in struct {
		Enabled               *bool `json:"enabled"`
		DefaultTTLSeconds     *int  `json:"default_ttl_seconds"`
		KeepOnAccountDisabled *bool `json:"keep_on_account_disabled"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if in.KeepOnAccountDisabled != nil {
		cur.KeepOnAccountDisabled = *in.KeepOnAccountDisabled
	}
	if in.DefaultTTLSeconds != nil {
		if v := *in.DefaultTTLSeconds; v < 1 || v > 30*24*3600 {
			msg := "must be between 1 and 2592000"
			if core.Locale(ctx) == "zh" {
				msg = "取值范围为 1 到 2592000"
			}
			httpapi.Fail(c, core.InvalidFields(core.FieldError{Field: "default_ttl_seconds", Code: "out_of_range", Message: msg}))
			return
		}
		cur.DefaultTTLSeconds = *in.DefaultTTLSeconds
	}
	uid, _ := core.UserID(ctx)
	if err := store.PatchSettingJSON(ctx, db, settingsKeySticky, nullID(uid), in, &cur); err != nil {
		httpapi.Fail(c, err)
		return
	}
	g.changed(ctx, "settings.sticky")
	httpapi.OK(c, cur)
}

// ---------------------------------------------------------------- default rules

// SyncBuiltinDefaults writes the default sticky rules of the built-in
// platforms (source=builtin): definitions follow the code unless an
// administrator edited them (customized, kept until restored to the
// default); the switches (enabled, priority) are always the administrator's.
// Called when the gateway starts.
func (g *Gateway) SyncBuiltinDefaults(ctx context.Context) error {
	if g.d.DB == nil {
		return nil
	}
	if err := g.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		return syncBuiltinRules(ctx, tx, builtinStickyRules())
	}); err != nil {
		return err
	}
	g.changed(ctx, "sticky_rules")
	return nil
}

// builtinStickyRules are the default sticky rules of the built-in platforms.
func builtinStickyRules() []manifest.StickyRule {
	var rules []manifest.StickyRule
	for _, p := range platforms.Builtin() {
		rules = append(rules, p.StickyRules...)
	}
	return rules
}

// syncBuiltinRules upserts rules as the built-in defaults, leaving the
// definition of a customized one alone, and deletes the built-in defaults no
// longer listed.
func syncBuiltinRules(ctx context.Context, tx *gorm.DB, rules []manifest.StickyRule) error {
	g := store.G(ctx, tx)
	// A rule of the name is rewritten unless an administrator customized it;
	// its switches (enabled, priority) stay as they are.
	upsert := clause.OnConflict{
		Columns: []clause.Column{{Name: "name"}, {Name: "source"}},
		DoUpdates: append(clause.AssignmentColumns([]string{"match", "key_sources", "value_regex", "ttl_seconds",
			"key_includes", "on_failure", "pass_headers"}), clause.Assignment{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("now()")}),
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "NOT sticky_rules.customized"}}},
	}
	names := make([]string, 0, len(rules))
	for i, r := range rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			return errors.New("sticky rule without name")
		}
		onFailure := r.OnFailure
		if onFailure != onFailureStick {
			onFailure = onFailureFailover
		}
		includes := r.KeyIncludes
		if len(includes) == 0 {
			includes = defaultKeyIncludes
		}
		row := &model.StickyRule{Name: name, Source: sourceBuiltin, Enabled: new(true), Priority: new(100 + i),
			Match: mustJSON(r.Match), KeySources: mustJSON(r.KeySources), ValueRegex: r.ValueRegex,
			TTLSeconds: new(max(r.TTLSeconds, 0)), KeyIncludes: mustJSON(includes), OnFailure: &onFailure,
			PassHeaders: nonNil(r.PassHeaders)}
		if err := g.Clauses(upsert).Create(row).Error; err != nil {
			return err
		}
		names = append(names, name)
	}
	return g.Where("source = 'builtin' AND NOT (name = ANY(?))", store.Strings(names)).Delete(&model.StickyRule{}).Error
}

// ruleDefinition is what tells whether a rule's definition changed (not its
// name or switches); empty and missing lists are the same.
func ruleDefinition(r *stickyRule) string {
	sources := r.KeySources
	if sources == nil {
		sources = []manifest.StickyKeySource{}
	}
	return string(mustJSON([]any{nonNil(r.Match.Protocols), nonNil(r.Match.Models), nonNil(r.Match.UserAgentContains),
		sources, r.ValueRegex, r.TTLSeconds, nonNil(r.KeyIncludes), r.OnFailure, nonNil(r.PassHeaders)}))
}

// resetRuleHandler restores a built-in rule to the core's default
// definition; its switches (enabled, priority) stay the administrator's.
func (g *Gateway) resetRuleHandler(c *gin.Context) {
	ctx := c.Request.Context()
	db, err := g.db()
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	r, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if r.Source == sourceAdmin {
		msg := "only built-in rules have a default to restore"
		if core.Locale(ctx) == "zh" {
			msg = "只有内置规则可以恢复默认"
		}
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage(msg))
		return
	}
	uid, _ := core.UserID(ctx)
	if err := db.Tx(ctx, func(tx *gorm.DB) error {
		// The definition goes back to the sync that follows, which writes
		// updated_at.
		if err := store.G(ctx, tx).Model(&model.StickyRule{}).Where("id = ?", id).
			Updates(map[string]any{"customized": false, "updated_by": nullID(uid)}).Error; err != nil {
			return err
		}
		return syncBuiltinRules(ctx, tx, builtinStickyRules())
	}); err != nil {
		httpapi.Fail(c, err)
		return
	}
	g.changed(ctx, "sticky_rules")
	updated, err := getRule(ctx, db.Gorm, id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, toDTO(updated))
}

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/lookup"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

const flowManage = "plugin.demo:flow:manage"

// scopeAuthz: admin (1) holds flow:manage and level:set unscoped with
// role:read; viewer (2) plugin:read with flows 3 and 4 and levels a and c;
// nobody (3) nothing; manager (4) role:read alone; runner (5) plugin:read
// with own flows. An access key narrows them as authz does.
type scopeAuthz struct{}

func (a scopeAuthz) Can(ctx context.Context, uid int64, p string) (bool, error) {
	set, err := a.PermissionSet(ctx, uid)
	return set.Has(p), err
}

func (scopeAuthz) CanGrant(context.Context, int64, []string) error { return nil }

func (scopeAuthz) PermissionSet(ctx context.Context, uid int64) (core.PermissionSet, error) {
	keys := map[int64][]string{
		1: {"role:read", flowManage, "plugin.demo:level:set"},
		2: {"plugin:read", flowManage + ":flow/3", flowManage + ":flow/4", "plugin.demo:level:set:value/a", "plugin.demo:level:set:value/c"},
		4: {"role:read"},
		5: {"plugin:read", flowManage + ":own"},
	}[uid]
	return core.RestrictToAccessKey(ctx, uid, core.PermissionSetOf(keys...)), nil
}

// optionsPlugin is the plugin's options route: it records the request and
// answers resp.
type optionsPlugin struct {
	last *pluginv1.HTTPRequest
	resp *pluginv1.HTTPResponse
}

func (p *optionsPlugin) HandleHTTP(_ context.Context, in *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	p.last = in
	return p.resp, nil
}

// optionsGen serves routes, nil while the plugin is not running.
type optionsGen struct {
	gen
	routes []core.RouteBinding
}

func (g *optionsGen) Routes(string) []core.RouteBinding { return g.routes }

type optionsRegistry struct{ g *optionsGen }

func (r optionsRegistry) Current() core.Generation                     { return r.g }
func (optionsRegistry) OnChange(func(core.Generation)) (cancel func()) { return func() {} }

type scopeEnv struct {
	t      *testing.T
	engine *gin.Engine
	gen    *optionsGen
	plugin *optionsPlugin
}

// newScopeEnv installs plugin demo (current version 1.0.0) with the scoped
// flow:manage (options /scopes/flow, own) and level:set (values a, b, c),
// not running until the test sets gen.routes.
func newScopeEnv(t *testing.T) *scopeEnv {
	db := testutil.DB(t)
	m := registrytest.Manifest("demo", "1.0.0")
	m.UserPermissions = append(m.UserPermissions,
		manifest.UserPermission{Key: "flow:manage", Label: manifest.LocalizedText{"en": "Manage flows"},
			Scope: &manifest.PermissionScope{Resource: "flow", Options: "/scopes/flow", Own: true}},
		manifest.UserPermission{Key: "level:set", Label: manifest.LocalizedText{"en": "Set level"},
			Scope: &manifest.PermissionScope{Values: []manifest.ScopeValue{
				{Key: "a", Label: manifest.LocalizedText{"en": "Alpha", "zh": "甲"}},
				{Key: "b", Label: manifest.LocalizedText{"en": "Beta", "zh": "乙"}},
				{Key: "c", Label: manifest.LocalizedText{"en": "Gamma"}},
			}}})
	m.Routes = append(m.Routes, optionsRoute)
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO plugins (key, name, status, active_version) VALUES ('demo', '{}', 'enabled', '1.0.0')`)
	mustExec(t, db, `INSERT INTO plugin_versions (plugin_key, version, manifest, manifest_hash, package_sha256, package_size, signature_status, consent_status)
		VALUES ('demo', '1.0.0', $1, 'h', 's', 1, 'unsigned', 'approved')`, raw)

	env := &scopeEnv{t: t, gen: &optionsGen{}, plugin: &optionsPlugin{}}
	gin.SetMode(gin.TestMode)
	env.engine = gin.New()
	r := httpapi.NewRouter(env.engine, tokens{}, scopeAuthz{})
	New(Deps{DB: db, Registry: optionsRegistry{env.gen}, Authz: scopeAuthz{}}).RegisterRoutes(r)
	return env
}

var optionsRoute = manifest.Route{Method: "GET", Path: "/scopes/flow", Scope: "admin", Permission: "flow:manage"}

// run starts the plugin with its options route.
func (e *scopeEnv) run() {
	e.gen.routes = []core.RouteBinding{{Plugin: core.PluginInfo{Key: "demo"}, Route: optionsRoute, Client: e.plugin}}
}

func optionsAnswer(t *testing.T, items []map[string]any, more bool) *pluginv1.HTTPResponse {
	body, err := json.Marshal(map[string]any{"data": map[string]any{"items": items, "more": more}})
	require.NoError(t, err)
	return &pluginv1.HTTPResponse{Status: http.StatusOK, Body: body}
}

// get asks for the options of permission with query (after "permission=").
func (e *scopeEnv) get(token, query, lang string) (int, lookup.Result, string) {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/demo/scope-options?permission="+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	var out struct {
		Data  lookup.Result `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Data, out.Error.Code
}

func optionIDs(res lookup.Result) []string {
	out := []string{}
	for _, it := range res.Items {
		out = append(out, it.ID.(string))
	}
	return out
}

// A values scope is answered from the manifest, the plugin not running:
// q matches a key or any label, names follow the request's language, ids
// and limit as GET /lookup, and only values in the caller's scope.
func TestScopeOptionsValues(t *testing.T) {
	e := newScopeEnv(t)
	tests := []struct {
		name, token, query, lang string
		ids                      []string
		more                     bool
	}{
		{"all", "admin", "level:set", "", []string{"a", "b", "c"}, false},
		{"q matches a key", "admin", "level:set&q=C", "", []string{"c"}, false},
		{"q matches a label in another language", "admin", "level:set&q=乙", "", []string{"b"}, false},
		{"limit", "admin", "level:set&limit=2", "", []string{"a", "b"}, true},
		{"ids", "admin", "level:set&ids=c,b&q=zzz&limit=1", "", []string{"b", "c"}, false},
		{"caller's scope", "viewer", "level:set", "", []string{"a", "c"}, false},
		{"ids outside the caller's scope", "viewer", "level:set&ids=b", "", []string{}, false},
		{"access key narrows", core.AccessKeyPrefix + "plugin:read,plugin.demo:level:set:value/a", "level:set", "", []string{"a"}, false},
		{"nothing held", "manager", "level:set", "", []string{}, false},
	}
	for _, tt := range tests {
		code, res, _ := e.get(tt.token, tt.query, tt.lang)
		if assert.Equal(t, http.StatusOK, code, tt.name) {
			assert.Equal(t, tt.ids, optionIDs(res), tt.name)
			assert.Equal(t, tt.more, res.More, tt.name)
		}
	}
	_, res, _ := e.get("admin", "level:set&ids=a", "zh-CN")
	assert.Equal(t, []lookup.Item{{ID: "a", Name: "甲"}}, res.Items)
	_, res, _ = e.get("admin", "level:set&ids=c", "zh-CN")
	assert.Equal(t, []lookup.Item{{ID: "c", Name: "Gamma"}}, res.Items)
}

// A resource scope asks the plugin's options route with the caller's scope
// of the permission configured; what comes back is cut to valid rows in
// that scope and to the limit. Nothing is asked when the scope reaches
// nothing or none of the ids.
func TestScopeOptionsForwards(t *testing.T) {
	e := newScopeEnv(t)
	e.run()
	long := strings.Repeat("é", 200)
	e.plugin.resp = optionsAnswer(t, []map[string]any{
		{"id": "3", "name": "Three", "hint": long, "status": strings.Repeat("s", 40)},
		{"id": "4", "name": "Four"},
		{"id": "9", "name": "Nine"},
		{"id": "bad id", "name": "Bad"},
		{"id": "5", "name": ""},
		{"id": 7, "name": "Number"},
	}, false)

	code, res, _ := e.get("viewer", "flow:manage&q=t", "zh")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{"3", "4"}, optionIDs(res))
	assert.Len(t, res.Items[0].Hint, 256)
	assert.Len(t, res.Items[0].Status, 32)
	assert.False(t, res.More)
	req := e.plugin.last
	assert.Equal(t, "/scopes/flow", req.GetPath())
	assert.Equal(t, int64(2), req.GetCaller().GetUserId())
	assert.Equal(t, []string{"t"}, req.GetQuery()["q"].GetValues())
	assert.Equal(t, []string{"20"}, req.GetQuery()["limit"].GetValues())
	assert.Equal(t, []string{"zh"}, req.GetHeaders()["accept-language"].GetValues())
	assert.True(t, proto.Equal(&pluginv1.ActionScope{Permission: flowManage, Values: []string{"3", "4"}}, req.GetActionScope()), "%v", req.GetActionScope())

	// own: the plugin filters by creator, so its rows are kept.
	_, res, _ = e.get("runner", "flow:manage", "")
	assert.Equal(t, []string{"3", "4", "9"}, optionIDs(res))
	assert.True(t, proto.Equal(&pluginv1.ActionScope{Permission: flowManage, Own: true}, e.plugin.last.GetActionScope()))

	_, res, _ = e.get("admin", "flow:manage&limit=2", "")
	assert.Equal(t, []string{"3", "4"}, optionIDs(res))
	assert.True(t, res.More)

	// ids are cut to the caller's values before asking.
	_, res, _ = e.get("viewer", "flow:manage&ids=3,9", "")
	assert.Equal(t, []string{"3"}, optionIDs(res))
	assert.Equal(t, []string{"3"}, e.plugin.last.GetQuery()["ids"].GetValues())

	for _, tt := range []struct{ name, token, query string }{
		{"nothing held", "manager", "flow:manage"},
		{"no id in scope", "viewer", "flow:manage&ids=9"},
	} {
		e.plugin.last = nil
		code, res, _ := e.get(tt.token, tt.query, "")
		assert.Equal(t, http.StatusOK, code, tt.name)
		assert.Empty(t, res.Items, tt.name)
		assert.Nil(t, e.plugin.last, tt.name)
	}
}

func TestScopeOptionsErrors(t *testing.T) {
	e := newScopeEnv(t)
	tests := []struct {
		name, token, query string
		status             int
		code               string
	}{
		{"unknown permission", "admin", "nope:x", 404, "not_found"},
		{"unscoped permission", "admin", "rules:read", 400, "invalid_argument"},
		{"neither role:read nor plugin:read", "nobody", "level:set", 403, "permission_denied"},
		{"invalid limit", "admin", "level:set&limit=0", 400, "invalid_argument"},
		{"plugin not running", "admin", "flow:manage", 503, "plugin_unavailable"},
	}
	for _, tt := range tests {
		status, _, code := e.get(tt.token, tt.query, "")
		assert.Equal(t, tt.status, status, tt.name)
		assert.Equal(t, tt.code, code, tt.name)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/ghost/scope-options?permission=level:set", nil)
	req.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code, "unknown plugin")

	e.run()
	for _, resp := range []*pluginv1.HTTPResponse{{Status: 500, Body: []byte(`{}`)}, {Status: 200, Body: []byte(`not json`)}} {
		e.plugin.resp = resp
		status, _, code := e.get("admin", "flow:manage", "")
		assert.Equal(t, 503, status, "%d %s", resp.Status, resp.Body)
		assert.Equal(t, "plugin_unavailable", code)
	}
}

// GET /ui/plugins shows a plugin's menu and slot to a holder of any scope
// of their permission.
func TestUIPluginsScopedPermission(t *testing.T) {
	h, _ := newHarness(t)
	menus := func(token string) []any {
		code, out := h.do("GET", "/ui/plugins", token, nil)
		require.Equal(t, 200, code, out)
		ui, _ := out["data"].([]any)
		if len(ui) == 0 {
			return nil
		}
		return ui[0].(map[string]any)["menus"].([]any)
	}
	// The viewer holds stats:read; its key narrows that to one value.
	assert.Len(t, menus(core.AccessKeyPrefix+"plugin.guard:stats:read:value/a,"+PermSelfUI), 1)
	assert.Empty(t, menus(core.AccessKeyPrefix+"plugin.guard:other:read:value/a,"+PermSelfUI))
}

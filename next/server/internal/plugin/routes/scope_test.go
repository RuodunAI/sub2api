package routes_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
)

const flowManage = "plugin.demo:flow:manage"

type scopeUser struct {
	id   int64
	keys []string
}

// scopeUsers are the callers of the scope tests, by token, with their
// grants of flow:manage.
var scopeUsers = map[string]scopeUser{
	"flow-all":   {10, []string{flowManage}},
	"flow-3":     {11, []string{flowManage + ":flow/3"}},
	"flow-5":     {12, []string{flowManage + ":flow/5"}},
	"flow-own":   {13, []string{flowManage + ":own"}},
	"flow-own-5": {14, []string{flowManage + ":own", flowManage + ":flow/5"}},
}

// scopeManifest declares the scoped flow:manage with a list route and
// routes naming the flow on a path, a query and a body.
func scopeManifest() *manifest.Manifest {
	m := registrytest.Manifest("demo", "1.0.0")
	m.UserPermissions = append(m.UserPermissions, manifest.UserPermission{Key: "flow:manage",
		Label: manifest.LocalizedText{"en": "Manage flows"}, Scope: &manifest.PermissionScope{Resource: "flow", Own: true}})
	m.Routes = append(m.Routes,
		manifest.Route{Method: "GET", Path: "/flows", Scope: "admin", Permission: "flow:manage"},
		manifest.Route{Method: "PUT", Path: "/flows/:id", Scope: "admin", Permission: "flow:manage", ScopeFrom: "path:id"},
		manifest.Route{Method: "GET", Path: "/pick", Scope: "admin", Permission: "flow:manage", ScopeFrom: "query:flow"},
		manifest.Route{Method: "POST", Path: "/pick", Scope: "admin", Permission: "flow:manage", ScopeFrom: "body:flow_ids"},
	)
	return m
}

// A route of a scoped permission admits any scope and forwards the
// caller's scope; the values its scopeFrom names on a path, a query or a
// body reach core.AuthorizePluginScope (whose rules are tested in core). An
// access key narrows the scope; a route of an unscoped permission forwards
// none. Query and body names match in any case; a body naming the field
// twice is refused, a field nested deeper counts for nothing.
func TestPluginRouteScopeFrom(t *testing.T) {
	e, h, _, _ := setupManifest(t, scopeManifest())
	all := &pluginv1.ActionScope{Permission: flowManage, All: true}
	own := &pluginv1.ActionScope{Permission: flowManage, Own: true}
	only3 := &pluginv1.ActionScope{Permission: flowManage, Values: []string{"3"}}
	tooMany := make([]string, manifest.MaxAccountBatch+1)
	for i := range tooMany {
		tooMany[i] = strconv.Itoa(i)
	}
	tests := []struct {
		name, method, path, token, body string
		status                          int
		scope                           *pluginv1.ActionScope // forwarded; nil = none
	}{
		{"list, own", "GET", "/flows", "flow-own", "", 201, own},
		{"list, all", "GET", "/flows", "flow-all", "", 201, all},
		{"list, nothing held", "GET", "/flows", "nobody", "", 403, nil},
		{"path, listed", "PUT", "/flows/3", "flow-3", "", 201, only3},
		{"path, unlisted", "PUT", "/flows/3", "flow-5", "", 404, nil},
		{"query, comma-separated", "GET", "/pick?flow=3,%203", "flow-3", "", 201, only3},
		{"query, every value", "GET", "/pick?flow=3&flow=5", "flow-3", "", 404, nil},
		{"query, other case", "GET", "/pick?FLOW=3", "flow-3", "", 201, only3},
		{"query, every case", "GET", "/pick?flow=3&FLOW=5", "flow-3", "", 404, nil},
		{"query, none named", "GET", "/pick", "flow-own", "", 403, nil},
		{"body, number array", "POST", "/pick", "flow-3", `{"flow_ids":[3]}`, 201, only3},
		{"body, string in other case", "POST", "/pick", "flow-3", `{"FLOW_IDS":"3"}`, 201, only3},
		{"body, mixed array", "POST", "/pick", "flow-3", `{"flow_ids":[3,"5"]}`, 404, nil},
		{"body, duplicate field", "POST", "/pick", "flow-3", `{"flow_ids":[5],"flow_ids":[3]}`, 400, nil},
		{"body, duplicate in other case", "POST", "/pick", "flow-3", `{"flow_ids":[3],"Flow_Ids":[3]}`, 400, nil},
		{"body, nested fields ignored", "POST", "/pick", "flow-3",
			`{"meta":{"flow_ids":[5],"x":[{"flow_ids":5}]},"flow_ids":[3],"list":[{"flow_ids":[5]}]}`, 201, only3},
		{"body, trailing data", "POST", "/pick", "flow-3", `{"flow_ids":[3]} {"flow_ids":[5]}`, 403, nil},
		{"body, none named", "POST", "/pick", "flow-own", `{}`, 403, nil},
		{"invalid query value", "GET", "/pick?flow=a/b", "flow-all", "", 400, nil},
		{"invalid body value", "POST", "/pick", "flow-all", `{"flow_ids":[true]}`, 400, nil},
		{"too many values", "GET", "/pick?flow=" + strings.Join(tooMany, ","), "flow-all", "", 400, nil},
		{"access key narrows all to own", "GET", "/flows", core.AccessKeyPrefix + "flow-all@flow:manage:own", "", 201, own},
		{"access key not within own", "GET", "/flows", core.AccessKeyPrefix + "flow-own@flow:manage:flow/2", "", 403, nil},
		{"unscoped permission", "GET", "/echo/1", "admin", "", 201, nil},
	}
	for _, tt := range tests {
		h.last = nil
		w := do(e, tt.method, "/api/v1/p/demo"+tt.path, tt.token, tt.body)
		if !assert.Equal(t, tt.status, w.Code, "%s: %s", tt.name, w.Body) || tt.status != 201 {
			assert.Nil(t, h.last, tt.name)
			continue
		}
		got := h.last.GetActionScope()
		if tt.scope == nil {
			assert.Nil(t, got, tt.name)
		} else {
			assert.True(t, proto.Equal(tt.scope, got), "%s: %v", tt.name, got)
		}
	}
	// A scopeFrom naming no value is denied naming the permission; a
	// duplicate body field is named.
	w := do(e, "GET", "/api/v1/p/demo/pick", "flow-own", "")
	assert.Contains(t, w.Body.String(), `"permission":"`+flowManage+`"`)
	w = do(e, "POST", "/api/v1/p/demo/pick", "flow-all", `{"flow_ids":[3],"flow_ids":[3]}`)
	assert.Contains(t, w.Body.String(), `"code":"duplicate"`)
}

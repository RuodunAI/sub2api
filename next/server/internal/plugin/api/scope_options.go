package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/lookup"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/routes"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/x"
)

// The bounds of a row an options route answers: lookup's text bound for
// name and hint.
const (
	maxOptionText   = 256
	maxOptionStatus = 32
)

// scopeOptions answers GET /plugins/:key/scope-options?permission=<local>:
// the candidates of a scoped plugin permission (manifest
// userPermissions[].scope) within the caller's own scope of it, in the shape
// of GET /lookup with text ids. A values scope is answered from the manifest
// of the plugin's current version, so it works while the plugin is not
// running; a resource scope asks the plugin's options route.
func (a *API) scopeOptions(c *gin.Context) {
	key, local := c.Param("key"), c.Query("permission")
	rc := c.Request.Context()
	m, _, err := a.loadCurrentManifest(rc, key)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	up, ok := m.FindUserPermission(local)
	if !ok {
		httpapi.Fail(c, core.ErrNotFound.WithMessage("permission not found"))
		return
	}
	if up.Scope == nil {
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("permission takes no scope"))
		return
	}
	q, err := lookup.ParseText(c)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	set, err := a.d.Authz.PermissionSet(rc, actor(c))
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	action := manifest.PermissionKey(key, local)
	ps := set.PluginScope(perm.Key(action))
	if len(up.Scope.Values) > 0 {
		httpapi.OK(c, valueOptions(rc, up.Scope.Values, ps, q))
		return
	}
	res, err := a.pluginOptions(c, key, up.Scope.Options, action, ps, q)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, res)
}

// valueOptions lists the values the caller reaches: the given ids among
// them, else those whose key or a label contains q (case-insensitively),
// up to the limit. A value is named by its label in the request's language.
func valueOptions(ctx context.Context, values []manifest.ScopeValue, ps core.PluginScope, q lookup.TextQuery) lookup.Result {
	res := lookup.Result{Items: []lookup.Item{}}
	needle := strings.ToLower(q.Q)
	for _, v := range values {
		label := core.LocalizedText(v.Label)
		match := slices.Contains(q.IDs, v.Key)
		if q.IDs == nil {
			match = strings.Contains(strings.ToLower(v.Key), needle)
			for _, s := range label {
				match = match || strings.Contains(strings.ToLower(s), needle)
			}
		}
		if !match || !ps.Allows(v.Key) {
			continue
		}
		if q.IDs == nil && len(res.Items) == q.Limit {
			res.More = true
			break
		}
		res.Items = append(res.Items, lookup.Item{ID: v.Key, Name: cmp.Or(label.Get(core.Locale(ctx)), v.Key)})
	}
	return res
}

// pluginOptions asks the plugin's options route (a GET admin route at path)
// for the candidates within ps, the caller's scope of action, the permission
// being configured, which the plugin gets as action_scope. Nothing is asked
// when ps reaches nothing or none of the ids; what the plugin answers is
// kept to valid rows in ps (the plugin is not trusted to filter).
func (a *API) pluginOptions(c *gin.Context, key, path, action string, ps core.PluginScope, q lookup.TextQuery) (lookup.Result, error) {
	res := lookup.Result{Items: []lookup.Item{}}
	ids, limit := q.IDs, q.Limit
	if ps.Empty() {
		return res, nil
	}
	if !ps.All && !ps.Own && ids != nil {
		if ids = slices.DeleteFunc(ids, func(id string) bool { return !ps.Allows(id) }); len(ids) == 0 {
			return res, nil
		}
	}
	rb, ok := a.optionsRoute(key, path)
	if !ok {
		return res, core.ErrPluginUnavailable.WithDetails(map[string]any{"plugin_key": key})
	}
	query := map[string]*pluginv1.HeaderValues{"q": {Values: []string{q.Q}}, "limit": {Values: []string{strconv.Itoa(limit)}}}
	if ids != nil {
		query["ids"] = &pluginv1.HeaderValues{Values: []string{strings.Join(ids, ",")}}
		limit = len(ids)
	}
	req := &pluginv1.HTTPRequest{Caller: routes.CallerOf(c), Method: http.MethodGet, Path: path, RoutePath: rb.Route.Path,
		Query: query, Headers: map[string]*pluginv1.HeaderValues{}, ActionScope: ps.Proto(action)}
	if lang := c.Request.Header.Values("Accept-Language"); len(lang) > 0 {
		req.Headers["accept-language"] = &pluginv1.HeaderValues{Values: lang}
	}
	rc := c.Request.Context()
	resp, err := rb.Client.HandleHTTP(rc, req)
	var out struct {
		Data struct {
			Items []lookup.Item `json:"items"`
			More  bool          `json:"more"`
		} `json:"data"`
	}
	if err == nil && resp.GetStatus() != http.StatusOK {
		err = fmt.Errorf("options route answered %d", resp.GetStatus())
	}
	if err == nil {
		err = json.Unmarshal(resp.GetBody(), &out)
	}
	if err != nil {
		slog.WarnContext(rc, "plugin scope options", "plugin", key, "path", path, "err", err)
		return res, core.ErrPluginUnavailable.WithDetails(map[string]any{"plugin_key": key})
	}
	cut := false
	for _, it := range out.Data.Items {
		id, _ := it.ID.(string)
		if !perm.ValidScopeValue(id) || it.Name == "" || !ps.Own && !ps.Allows(id) {
			continue
		}
		if len(res.Items) == limit {
			cut = true
			break
		}
		res.Items = append(res.Items, lookup.Item{ID: id, Name: x.Truncate(it.Name, maxOptionText),
			Hint: x.Truncate(it.Hint, maxOptionText), Status: x.Truncate(it.Status, maxOptionStatus)})
	}
	res.More = ids == nil && (out.Data.More || cut)
	return res, nil
}

// optionsRoute is the running plugin's GET route at path; false while the
// plugin is not running or its running version has no such route.
func (a *API) optionsRoute(key, path string) (core.RouteBinding, bool) {
	if a.d.Registry == nil || a.d.Registry.Current() == nil {
		return core.RouteBinding{}, false
	}
	for _, rb := range a.d.Registry.Current().Routes(key) {
		if strings.EqualFold(rb.Route.Method, http.MethodGet) && rb.Route.Path == path {
			return rb, true
		}
	}
	return core.RouteBinding{}, false
}

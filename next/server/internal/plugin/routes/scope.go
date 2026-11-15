package routes

import (
	"fmt"
	"net/url"
	"slices"

	"github.com/gin-gonic/gin"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// maxScopeValues caps the values a scopeFrom names, the bound of a batch of
// account ids.
const maxScopeValues = manifest.MaxAccountBatch

var errTooManyScopeValues = core.ErrInvalidArgument.WithMessage(fmt.Sprintf("at most %d scope values per request", maxScopeValues))

// authorizeRoute checks the route's plugin permission key at some scope
// (PermissionSet.HasSome) and returns the caller's set for authorizeScope.
// A route without a permission names "" and is denied. A key held for
// every row is recorded in Granted, as CanAny does.
func (h *Handler) authorizeRoute(c *gin.Context, key string) (core.PermissionSet, bool) {
	ctx := c.Request.Context()
	uid, _ := core.UserID(ctx)
	set, err := h.authz.PermissionSet(ctx, uid)
	if err != nil {
		httpapi.Fail(c, err)
		return set, false
	}
	// "" parses as an action every superuser holds.
	if key == "" || !set.HasSome(key) {
		httpapi.Fail(c, core.ErrPermissionDenied.WithDetails(map[string]any{"permission": key}))
		return set, false
	}
	if set.Has(key) {
		c.Request = c.Request.WithContext(core.WithGranted(ctx, []string{key}))
	}
	return set, true
}

// authorizeScope applies the route permission's declared scope: nil and
// true for a permission without one; otherwise the caller's PluginScope,
// checked against the values the route's scopeFrom names
// (core.AuthorizePluginScope), as the ActionScope forwarded to the plugin.
func (h *Handler) authorizeScope(c *gin.Context, rb *core.RouteBinding, set core.PermissionSet,
	params map[string]string, body []byte) (*pluginv1.ActionScope, bool) {
	m := rb.Plugin.Manifest
	if m == nil || m.UserPermissionScope(rb.Route.Permission) == nil {
		return nil, true
	}
	key := manifest.PermissionKey(rb.Plugin.Key, rb.Route.Permission)
	scope := set.PluginScope(perm.Key(key))
	if rb.Route.ScopeFrom != "" {
		values, err := scopeValues(rb.Route.ScopeFrom, params, c.Request.URL.Query(), body)
		if err == nil {
			err = core.AuthorizePluginScope(c.Request.Context(), scope, key, values)
		}
		if err != nil {
			httpapi.Fail(c, err)
			return nil, false
		}
	}
	return scope.Proto(key), true
}

// scopeValues reads the distinct values a scopeFrom entry names
// (requestValues), each a plugin scope value (perm.ValidScopeValue), at
// most maxScopeValues.
func scopeValues(from string, params map[string]string, query url.Values, body []byte) ([]string, error) {
	values, err := requestValues(from, params, query, body)
	if err == nil && slices.ContainsFunc(values, func(v string) bool { return !perm.ValidScopeValue(v) }) {
		err = errBadValue
	}
	if err != nil {
		_, name, _ := manifest.ParseAccountsFrom(from)
		return nil, core.InvalidFields(core.FieldError{Field: name, Code: "invalid",
			Message: "must be an id of letters, digits, '_', '.' or '-' (at most 64), a list of them or a comma-separated string of them"})
	}
	slices.Sort(values)
	if values = slices.Compact(values); len(values) > maxScopeValues {
		return nil, errTooManyScopeValues
	}
	return values, nil
}

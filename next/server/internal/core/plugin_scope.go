package core

import (
	"context"
	"slices"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// PluginScope is what a console user reaches with one scoped plugin action
// (CONTRACTS §21.0, plugin resources): every row, the rows they created
// (Own: only the plugin knows its creator), and/or the listed values (the
// <value> of "<res>/<value>" grants, sorted, deduplicated). A plugin action
// ranges over one resource, which authz checks when a grant is written, so
// the values need no resource.
type PluginScope struct {
	All, Own bool
	Values   []string
}

// Empty reports that the user holds the action at no scope.
func (s PluginScope) Empty() bool { return !s.All && !s.Own && len(s.Values) == 0 }

// Allows reports whether value is in reach without asking the plugin: All,
// or a listed value. An Own scope is the plugin's to decide.
func (s PluginScope) Allows(value string) bool { return s.All || slices.Contains(s.Values, value) }

// Proto is s as the plugin sees it for action (qualified, without scope).
func (s PluginScope) Proto(action string) *pluginv1.ActionScope {
	return &pluginv1.ActionScope{Permission: action, All: s.All, Own: s.Own, Values: s.Values}
}

// PluginScope is the holder's scope of a plugin action: All for a superuser
// or an unscoped grant, else the own flag and the values of its scoped
// grants.
func (s PermissionSet) PluginScope(action perm.Key) PluginScope {
	if _, ok := s.Keys[string(action)]; ok || s.Superuser {
		return PluginScope{All: true}
	}
	var out PluginScope
	for _, sc := range s.Scoped[action] {
		out.Own = out.Own || sc.Own
		if sc.Value != "" {
			out.Values = append(out.Values, sc.Value)
		}
	}
	slices.Sort(out.Values)
	out.Values = slices.Compact(out.Values)
	return out
}

// HasSome reports whether the set holds k at some scope when k names none
// (HasAction), else whether it implies k (Has). What a plugin route, menu
// or slot naming a plugin key asks: a holder of x:own reaches part of x's
// rows. For an action that takes no scope it is Has.
func (s PermissionSet) HasSome(k string) bool {
	p, err := perm.Parse(k)
	if err != nil {
		return false
	}
	if p.Scope.All {
		return s.HasAction(p.Action)
	}
	return s.Has(k)
}

// AuthorizePluginScope is the scopeFrom rule, mirroring AuthorizeAccounts:
// All passes; otherwise the request must name a value (else
// ErrPermissionDenied naming key) and every value must be listed, unless
// the holder has Own, when values outside the list are the plugin's to
// check against its created_by (else ErrNotFound: out of scope looks like
// missing).
func AuthorizePluginScope(ctx context.Context, s PluginScope, key string, values []string) error {
	switch {
	case s.All:
		return nil
	case len(values) == 0:
		return ErrPermissionDenied.WithDetails(map[string]any{"permission": key})
	case s.Own:
		return nil
	case slices.ContainsFunc(values, func(v string) bool { return !s.Allows(v) }):
		return ErrNotFound.WithMessage(Text(ctx, "not found", "不存在"))
	}
	return nil
}

package manifest

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// BuiltinRoleKeys are the built-in roles a plugin permission may name in
// defaults (the core's built-in roles but the super admin; a core test
// keeps the lists in step).
var BuiltinRoleKeys = []string{"admin", "operator", "supplier", "user"}

// DefaultPermissionRoles are the roles of a permission declaring no
// defaultRoles.
var DefaultPermissionRoles = []string{"admin"}

// Roles reports the built-in roles the permission is granted to when first
// created: DefaultRoles, DefaultPermissionRoles when omitted (nil), none for
// an explicit empty list.
func (up UserPermission) Roles() []string {
	if up.DefaultRoles == nil {
		return slices.Clone(DefaultPermissionRoles)
	}
	return slices.Clone(up.DefaultRoles)
}

// UserPermissionRisks are the risks a user permission may declare.
var UserPermissionRisks = []string{RiskLow, RiskSensitive, RiskCritical}

// ScopeValueResource is the resource of a values scope: its grants are
// "value/<key>".
const ScopeValueResource = "value"

// Scope text patterns; server/internal/perm parses the same (a perm test
// keeps them equal).
const (
	ScopeResourcePattern = `^[a-z][a-z0-9_]{0,29}$`
	ScopeValuePattern    = `^[A-Za-z0-9_.-]{1,64}$`
)

// ReservedScopeResources may not be a plugin scope resource: the core's own
// scope resources (a plugin scopes accounts through routes[].accounts) and
// the values resource.
var ReservedScopeResources = []string{"account", "proxy", "group", ScopeValueResource}

// RoleDefault is one value of UserPermission.Defaults: JSON true (Unscoped)
// or an array of scopes. JSON false decodes to the zero value, which check
// rejects with a field error; any other JSON type fails to decode.
type RoleDefault struct {
	Unscoped bool
	Scopes   []string
}

// MarshalJSON writes Scopes when set (an empty list included), else
// Unscoped.
func (d RoleDefault) MarshalJSON() ([]byte, error) {
	if d.Scopes != nil {
		return json.Marshal(d.Scopes)
	}
	return json.Marshal(d.Unscoped)
}

func (d *RoleDefault) UnmarshalJSON(b []byte) error {
	var unscoped bool
	if json.Unmarshal(b, &unscoped) == nil {
		*d = RoleDefault{Unscoped: unscoped}
		return nil
	}
	var scopes []string
	if json.Unmarshal(b, &scopes) != nil {
		return errors.New("a role default is true or an array of scopes")
	}
	*d = RoleDefault{Scopes: scopes}
	return nil
}

// RiskLevel is up.Risk, RiskLow when omitted.
func (up UserPermission) RiskLevel() string {
	if up.Risk == "" {
		return RiskLow
	}
	return up.Risk
}

// ResourceName is the resource of the scope's grants: Resource, or
// ScopeValueResource for a values scope.
func (s PermissionScope) ResourceName() string {
	if len(s.Values) > 0 {
		return ScopeValueResource
	}
	return s.Resource
}

// GrantSuffix is the canonical stored scope of one defaults entry v: "*" is
// "" (every row), a values key k is "value/k", "own" is "own" when s.Own;
// false for anything else (ids of an options resource are runtime data and
// never a default).
func (s PermissionScope) GrantSuffix(v string) (string, bool) {
	switch {
	case v == "*":
		return "", true
	case len(s.Values) > 0:
		ok := slices.ContainsFunc(s.Values, func(sv ScopeValue) bool { return sv.Key == v })
		return ScopeValueResource + "/" + v, ok
	case v == "own":
		return v, s.Own
	}
	return "", false
}

// DefaultGrants maps each role of up.Defaults to its canonical suffixes
// (GrantSuffix), sorted and deduplicated, [""] alone when it holds "" since
// every row implies the rest. nil for a critical permission. Entries check
// rejects are skipped, so callers of a checked manifest need no errors.
func (up UserPermission) DefaultGrants() map[string][]string {
	if len(up.Defaults) == 0 || up.RiskLevel() == RiskCritical {
		return nil
	}
	var scope PermissionScope
	if up.Scope != nil {
		scope = *up.Scope
	}
	out := make(map[string][]string, len(up.Defaults))
	for role, d := range up.Defaults {
		var suffixes []string
		if d.Unscoped {
			suffixes = append(suffixes, "")
		}
		for _, v := range d.Scopes {
			if suffix, ok := scope.GrantSuffix(v); ok {
				suffixes = append(suffixes, suffix)
			}
		}
		switch {
		case len(suffixes) == 0:
			continue
		case slices.Contains(suffixes, ""):
			suffixes = []string{""}
		default:
			slices.Sort(suffixes)
			suffixes = slices.Compact(suffixes)
		}
		out[role] = suffixes
	}
	return out
}

// FindUserPermission returns the declared permission with local key local.
func (m *Manifest) FindUserPermission(local string) (UserPermission, bool) {
	i := slices.IndexFunc(m.UserPermissions, func(up UserPermission) bool { return up.Key == local })
	if i < 0 {
		return UserPermission{}, false
	}
	return m.UserPermissions[i], true
}

// UserPermissionScope is the scope of local, nil when undeclared or unscoped.
func (m *Manifest) UserPermissionScope(local string) *PermissionScope {
	up, _ := m.FindUserPermission(local)
	return up.Scope
}

// PermissionModule is the RBAC module of a plugin's permissions:
// "plugin.<pluginKey>".
func PermissionModule(pluginKey string) string { return "plugin." + pluginKey }

// PermissionKey is the RBAC key of a plugin-local permission:
// "plugin.<pluginKey>:<local>". An already qualified key of the same plugin is
// returned as is; an empty local key gives "" (callers then deny).
func PermissionKey(pluginKey, local string) string {
	prefix := PermissionModule(pluginKey) + ":"
	local = strings.TrimPrefix(local, prefix)
	if local == "" {
		return ""
	}
	return prefix + local
}

// LocalPermission reports the local part of key when it belongs to pluginKey.
func LocalPermission(pluginKey, key string) (string, bool) {
	local, ok := strings.CutPrefix(key, PermissionModule(pluginKey)+":")
	return local, ok && local != ""
}

package manifest

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPermissionKey(t *testing.T) {
	cases := []struct{ name, local, want string }{
		{"local", "orders:read", "plugin.shop:orders:read"},
		{"own prefix", "plugin.shop:orders:read", "plugin.shop:orders:read"},
		{"other plugin prefix", "plugin.blog:posts:read", "plugin.shop:plugin.blog:posts:read"},
		{"empty", "", ""},
		{"own prefix only", "plugin.shop:", ""},
	}
	for _, c := range cases {
		if got := PermissionKey("shop", c.local); got != c.want {
			t.Errorf("%s: PermissionKey(shop, %q) = %q, want %q", c.name, c.local, got, c.want)
		}
	}
}

func TestPermissionModule(t *testing.T) {
	if got := PermissionModule("shop"); got != "plugin.shop" {
		t.Fatalf("PermissionModule(shop) = %q", got)
	}
}

func TestLocalPermission(t *testing.T) {
	cases := []struct {
		name, key, want string
		ok              bool
	}{
		{"own", "plugin.shop:orders:read", "orders:read", true},
		{"other plugin", "plugin.blog:posts:read", "", false},
		{"plugin key prefix of another", "plugin.shopx:orders:read", "", false},
		{"core", "user:read", "", false},
		{"bare local", "orders:read", "", false},
		{"empty local", "plugin.shop:", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := LocalPermission("shop", c.key)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: LocalPermission(shop, %q) = %q, %v, want %q, %v", c.name, c.key, got, ok, c.want, c.ok)
		}
	}
}

// true and arrays survive a round trip, false decodes to the zero value
// (check reports it per field) and any other JSON type fails to decode.
func TestRoleDefaultJSON(t *testing.T) {
	for _, raw := range []string{`true`, `["*","own"]`, `[]`} {
		var d RoleDefault
		require.NoError(t, json.Unmarshal([]byte(raw), &d), raw)
		again, err := json.Marshal(d)
		require.NoError(t, err)
		assert.JSONEq(t, raw, string(again))
	}
	var d RoleDefault
	require.NoError(t, json.Unmarshal([]byte(`false`), &d))
	assert.Equal(t, RoleDefault{}, d)
	for _, raw := range []string{`"x"`, `{}`, `[1]`} {
		assert.Error(t, json.Unmarshal([]byte(raw), &d), raw)
	}
	var up UserPermission
	require.NoError(t, json.Unmarshal([]byte(`{"key":"a:b","defaults":{"admin":true,"user":["own"]}}`), &up))
	assert.Equal(t, map[string]RoleDefault{"admin": {Unscoped: true}, "user": {Scopes: []string{"own"}}}, up.Defaults)
}

func TestDefaultGrants(t *testing.T) {
	resource := &PermissionScope{Resource: "flow", Options: "/scopes/flow", Own: true}
	values := &PermissionScope{Values: []ScopeValue{{Key: "input"}, {Key: "output"}}}
	cases := []struct {
		name string
		up   UserPermission
		want map[string][]string
	}{
		{"unscoped", UserPermission{Defaults: map[string]RoleDefault{"admin": {Unscoped: true}}}, map[string][]string{"admin": {""}}},
		{"every row implies the rest", UserPermission{Scope: resource, Defaults: map[string]RoleDefault{"admin": {Scopes: []string{"own", "*"}}}}, map[string][]string{"admin": {""}}},
		{"own", UserPermission{Scope: resource, Defaults: map[string]RoleDefault{"operator": {Scopes: []string{"own"}}}}, map[string][]string{"operator": {"own"}}},
		{"values sorted and deduplicated", UserPermission{Scope: values, Defaults: map[string]RoleDefault{"admin": {Scopes: []string{"output", "input", "input"}}}}, map[string][]string{"admin": {"value/input", "value/output"}}},
		{"invalid entries skipped", UserPermission{Scope: values, Defaults: map[string]RoleDefault{"admin": {Scopes: []string{"other"}}, "user": {Scopes: []string{"input", "own"}}}}, map[string][]string{"user": {"value/input"}}},
		{"critical", UserPermission{Risk: RiskCritical, Defaults: map[string]RoleDefault{"admin": {Unscoped: true}}}, nil},
		{"no defaults", UserPermission{}, nil},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.up.DefaultGrants(), c.name)
	}
}

func TestGrantSuffix(t *testing.T) {
	resource := PermissionScope{Resource: "flow", Options: "/scopes/flow"}
	values := PermissionScope{Values: []ScopeValue{{Key: "input"}, {Key: "own"}}}
	cases := []struct {
		name  string
		scope PermissionScope
		in    string
		want  string
		ok    bool
	}{
		{"every row", resource, "*", "", true},
		{"own not allowed", resource, "own", "", false},
		{"own allowed", PermissionScope{Resource: "flow", Own: true}, "own", "own", true},
		{"an id is runtime data", resource, "flow/3", "", false},
		{"values key", values, "input", "value/input", true},
		{"values key named own", values, "own", "value/own", true},
		{"undeclared value", values, "output", "", false},
		{"values every row", values, "*", "", true},
	}
	for _, c := range cases {
		got, ok := c.scope.GrantSuffix(c.in)
		assert.Equal(t, c.ok, ok, c.name)
		if ok {
			assert.Equal(t, c.want, got, c.name)
		}
	}
}

func TestScopeTakes(t *testing.T) {
	resource := PermissionScope{Resource: "flow", Options: "/scopes/flow"}
	values := PermissionScope{Resource: ScopeValueResource, Values: []ScopeValue{{Key: "input"}}}
	cases := []struct {
		name  string
		scope PermissionScope
		sfx   string
		want  bool
	}{
		{"every row", PermissionScope{}, "", true},
		{"unscoped takes nothing else", PermissionScope{}, "own", false},
		{"own not allowed", resource, "own", false},
		{"own allowed", PermissionScope{Resource: "flow", Own: true}, "own", true},
		{"an id of its resource", resource, "flow/3", true},
		{"another resource", resource, "doc/3", false},
		{"no value", resource, "flow/", false},
		{"declared value", values, "value/input", true},
		{"value no longer declared", values, "value/output", false},
		{"values scope with Resource unset", PermissionScope{Values: values.Values}, "value/input", true},
		{"manifest spelling is not a suffix", values, "input", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.scope.Takes(c.sfx), c.name)
	}
}

func TestCanonicalSuffixes(t *testing.T) {
	in := []string{"value/b", "value/a", "value/b"}
	assert.Equal(t, []string{"value/a", "value/b"}, CanonicalSuffixes(in))
	assert.Equal(t, []string{"value/b", "value/a", "value/b"}, in, "the input stays")
	assert.Equal(t, []string{""}, CanonicalSuffixes([]string{"own", ""}))
	assert.Nil(t, CanonicalSuffixes([]string{}))
}

func TestUserPermissionScopeLookup(t *testing.T) {
	scope := &PermissionScope{Resource: "flow", Own: true}
	m := &Manifest{UserPermissions: []UserPermission{{Key: "flow:read"}, {Key: "flow:manage", Scope: scope}}}
	up, ok := m.FindUserPermission("flow:manage")
	require.True(t, ok)
	assert.Equal(t, scope, up.Scope)
	assert.Same(t, scope, m.UserPermissionScope("flow:manage"))
	assert.Nil(t, m.UserPermissionScope("flow:read"))
	assert.Nil(t, m.UserPermissionScope("flow:missing"))
	_, ok = m.FindUserPermission("flow:missing")
	assert.False(t, ok)
}

func TestScopeResourceName(t *testing.T) {
	assert.Equal(t, "flow", PermissionScope{Resource: "flow", Own: true}.ResourceName())
	assert.Equal(t, ScopeValueResource, PermissionScope{Values: []ScopeValue{{Key: "input"}}}.ResourceName())
	assert.Equal(t, RiskLow, UserPermission{}.RiskLevel())
	assert.Equal(t, RiskSensitive, UserPermission{Risk: RiskSensitive}.RiskLevel())
}

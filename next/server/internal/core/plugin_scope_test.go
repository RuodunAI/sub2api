package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// PluginScope folds the grants of one plugin action: All for an unscoped
// grant or a superuser, else own and the sorted, distinct values; grants of
// another action never leak in.
func TestPluginScope(t *testing.T) {
	t.Parallel()
	const action = "plugin.p:flow:manage"
	tests := []struct {
		name string
		set  PermissionSet
		want PluginScope
	}{
		{"unscoped grant", PermissionSetOf(action, action+":own"), PluginScope{All: true}},
		{"superuser", PermissionSet{Superuser: true}, PluginScope{All: true}},
		{"own and values", PermissionSetOf(action+":own", action+":flow/3", action+":flow/2", action+":flow/3"),
			PluginScope{Own: true, Values: []string{"2", "3"}}},
		{"other action only", PermissionSetOf("plugin.p:flow:read:flow/3", "plugin.q:flow:manage"), PluginScope{}},
	}
	for _, tt := range tests {
		got := tt.set.PluginScope(action)
		assert.Equal(t, tt.want, got, tt.name)
		assert.Equal(t, tt.want.Empty(), got.Empty(), tt.name)
	}
	assert.True(t, PluginScope{}.Empty())
	assert.Equal(t, "plugin.p:flow:manage", PluginScope{Own: true}.Proto(action).GetPermission())
}

// HasSome asks for some scope of an unscoped key and the implied scope of a
// scoped one; for keys that take no scope it is Has.
func TestHasSome(t *testing.T) {
	t.Parallel()
	const action = "plugin.p:flow:manage"
	own := PermissionSetOf(action+":own", "user:read", "account:read:own")
	assert.True(t, own.HasSome(action))
	assert.True(t, own.HasSome(action+":own"))
	assert.False(t, own.HasSome(action+":flow/3"))
	assert.False(t, own.HasSome("plugin.p:flow:read"))
	assert.True(t, PermissionSetOf(action).HasSome(action+":flow/3"))
	assert.False(t, own.HasSome("plugin.p:x:flow/bad value"))
	for _, k := range []string{"user:read", "user:update", "plugin.p:other"} {
		assert.Equal(t, own.Has(k), own.HasSome(k), k)
	}
}

// AuthorizePluginScope follows the scopeFrom matrix: All passes, no value
// is 403 naming the key, own defers to the plugin, a value out of the list
// is 404.
func TestAuthorizePluginScope(t *testing.T) {
	t.Parallel()
	const key = "plugin.p:flow:manage"
	tests := []struct {
		name   string
		scope  PluginScope
		values []string
		code   string // "" = allowed
	}{
		{"all", PluginScope{All: true}, []string{"9"}, ""},
		{"all without values", PluginScope{All: true}, nil, ""},
		{"listed", PluginScope{Values: []string{"3"}}, []string{"3"}, ""},
		{"one unlisted", PluginScope{Values: []string{"3"}}, []string{"3", "5"}, "not_found"},
		{"own reaches unlisted", PluginScope{Own: true, Values: []string{"5"}}, []string{"3"}, ""},
		{"no value", PluginScope{Own: true}, nil, "permission_denied"},
		{"nothing held", PluginScope{}, []string{"3"}, "not_found"},
	}
	for _, tt := range tests {
		err := AuthorizePluginScope(context.Background(), tt.scope, key, tt.values)
		assert.Equal(t, tt.code, codeOf(err), tt.name)
		if tt.code == "permission_denied" {
			assert.Equal(t, key, AsError(err).Details["permission"], tt.name)
		}
	}
}

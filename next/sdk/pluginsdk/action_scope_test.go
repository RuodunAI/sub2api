package pluginsdk_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

func scopedReq(uid int64, scope *pluginv1.ActionScope) *pluginv1.HTTPRequest {
	req := userReq(uid)
	req.ActionScope = scope
	return req
}

// A request without action_scope reaches nothing; All reaches every row,
// Values theirs, Own the rows the caller created (never a row without a
// creator).
func TestScopeAllows(t *testing.T) {
	t.Parallel()
	assert.False(t, pluginsdk.RequestScope(userReq(7)).Allows("3", 7))
	tests := []struct {
		name      string
		scope     *pluginv1.ActionScope
		value     string
		createdBy int64
		want      bool
	}{
		{"all", &pluginv1.ActionScope{All: true}, "3", 0, true},
		{"listed", &pluginv1.ActionScope{Values: []string{"3"}}, "3", 0, true},
		{"unlisted", &pluginv1.ActionScope{Values: []string{"3"}}, "4", 7, false},
		{"own, created by the caller", &pluginv1.ActionScope{Own: true}, "4", 7, true},
		{"own, created by another", &pluginv1.ActionScope{Own: true}, "4", 8, false},
		{"own, no creator", &pluginv1.ActionScope{Own: true}, "4", 0, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, pluginsdk.RequestScope(scopedReq(7, tt.scope)).Allows(tt.value, tt.createdBy), tt.name)
	}
	assert.False(t, pluginsdk.RequestScope(scopedReq(0, &pluginv1.ActionScope{Own: true})).Allows("4", 0))
}

func TestRequireScope(t *testing.T) {
	t.Parallel()
	own := &pluginv1.ActionScope{Own: true}
	assert.Equal(t, int32(http.StatusUnauthorized), pluginsdk.RequireScope(scopedReq(0, own), "4", 7).GetStatus())
	assert.Equal(t, int32(http.StatusNotFound), pluginsdk.RequireScope(scopedReq(7, own), "4", 8).GetStatus())
	assert.Nil(t, pluginsdk.RequireScope(scopedReq(7, own), "4", 7))
}

// ScopeOptions parses q, ids and limit, hands fn the caller's scope, cuts
// fn's rows to the limit and answers {data: {items, more}}; without a user
// fn is not called.
func TestScopeOptionsHandler(t *testing.T) {
	t.Parallel()
	var got pluginsdk.ScopeOptionsQuery
	rows := []pluginsdk.ScopeOption{{ID: "1", Name: "One"}, {ID: "2", Name: "Two", Hint: "h"}, {ID: "3", Name: "Three"}}
	h := pluginsdk.ScopeOptions(func(_ context.Context, q pluginsdk.ScopeOptionsQuery) ([]pluginsdk.ScopeOption, bool, error) {
		got = q
		return rows, false, nil
	})
	call := func(req *pluginv1.HTTPRequest, query map[string]string) (int32, map[string]any) {
		req.Query = map[string]*pluginv1.HeaderValues{}
		for k, v := range query {
			req.Query[k] = &pluginv1.HeaderValues{Values: []string{v}}
		}
		resp, err := h(context.Background(), req)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(resp.GetBody(), &out))
		data, _ := out["data"].(map[string]any)
		return resp.GetStatus(), data
	}

	status, data := call(scopedReq(7, &pluginv1.ActionScope{Permission: "plugin.p:flow:manage", Own: true}), map[string]string{"q": " th ", "limit": "2"})
	assert.Equal(t, int32(200), status)
	assert.Equal(t, "th", got.Text)
	assert.Equal(t, 2, got.Limit)
	assert.True(t, got.Scope.Own)
	assert.Equal(t, "plugin.p:flow:manage", got.Scope.Permission)
	assert.Len(t, data["items"], 2)
	assert.Equal(t, true, data["more"])

	_, data = call(userReq(7), map[string]string{"limit": "500"})
	assert.Equal(t, 20, got.Limit)
	assert.Equal(t, false, data["more"])

	_, data = call(userReq(7), map[string]string{"ids": "2, 1", "q": "x"})
	assert.Equal(t, []string{"2", "1"}, got.IDs)
	assert.Equal(t, "", got.Text)
	assert.Len(t, data["items"], 2)
	assert.Equal(t, false, data["more"])

	got = pluginsdk.ScopeOptionsQuery{}
	status, _ = call(userReq(0), nil)
	assert.Equal(t, int32(401), status)
	assert.Zero(t, got.Limit)
}

package group

import (
	"context"
	"fmt"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// fakeProtocols answers core.GroupProtocols and records the groups asked.
type fakeProtocols struct {
	served []core.GroupServedProtocol
	err    error
	asked  []int64
}

func (f *fakeProtocols) GroupServedProtocols(_ context.Context, groupID int64) ([]core.GroupServedProtocol, error) {
	f.asked = append(f.asked, groupID)
	return f.served, f.err
}

// GET /groups/:id adds served_protocols, what the account module reports
// for the group, to the group's fields; the list and the writes do not.
func TestGroupDetailServedProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.DB(t)
	e := &env{t: t, db: db, admin: mkUser(t, db, "admin@x.com")}
	svc := New(db, nil, nil, fakeRegistry{gen: testGen()}, authz.New(authz.Deps{}))
	engine := gin.New()
	svc.RegisterRoutes(httpapi.NewRouter(engine, fakeTokens{}, fakeAuthz{admin: e.admin}))
	e.h = engine

	code, out := e.do(e.admin, "POST", "/groups", map[string]any{"name": "Default"})
	require.Equal(t, 201, code, fmt.Sprint(out))
	created := out["data"].(map[string]any)
	require.NotContains(t, created, "served_protocols")
	id := int64(created["id"].(float64))
	path := fmt.Sprintf("/groups/%d", id)

	// Without the account module: an empty list, never null.
	code, out = e.do(e.admin, "GET", path, nil)
	require.Equal(t, 200, code, fmt.Sprint(out))
	detail := out["data"].(map[string]any)
	require.Equal(t, []any{}, detail["served_protocols"])
	require.Equal(t, "Default", detail["name"], "the group's fields stay at the top level")
	require.Equal(t, []any{}, detail["platforms"])

	reporter := &fakeProtocols{served: []core.GroupServedProtocol{{Protocol: "openai.chat", Accounts: 3}, {Protocol: "anthropic.messages", Accounts: 1}}}
	svc.Protocols = reporter
	code, out = e.do(e.admin, "GET", path, nil)
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.Equal(t, []any{
		map[string]any{"protocol": "openai.chat", "accounts": float64(3)},
		map[string]any{"protocol": "anthropic.messages", "accounts": float64(1)},
	}, out["data"].(map[string]any)["served_protocols"])
	require.Equal(t, []int64{id}, reporter.asked)

	code, out = e.do(e.admin, "GET", "/groups", nil)
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.NotContains(t, out["data"].([]any)[0], "served_protocols")
	code, out = e.do(e.admin, "PATCH", path, map[string]any{"description": "d"})
	require.Equal(t, 200, code, fmt.Sprint(out))
	require.NotContains(t, out["data"], "served_protocols")
	require.Equal(t, []int64{id}, reporter.asked, "only the detail asks")

	// A missing group is not asked about; a failed read fails the request.
	code, _ = e.do(e.admin, "GET", "/groups/999999", nil)
	require.Equal(t, 404, code)
	require.Equal(t, []int64{id}, reporter.asked)
	reporter.err = core.ErrUnavailable
	code, _ = e.do(e.admin, "GET", path, nil)
	require.Equal(t, 503, code)
}

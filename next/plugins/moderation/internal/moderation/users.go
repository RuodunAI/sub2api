package moderation

import (
	"context"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Users of the core (HostService.GetUsers, grant users.read,
// CONTRACTS §75.3): a user must exist before it is put on the watch list or
// banned (new-api AddModerationWatch), and the console shows names for the
// ids of a page with one call (GET /users?ids=).

// CoreUser is one user of GET /users: no email or roles, which only the
// core's GET /users/:id shows (user:read).
type CoreUser struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// lookupUsers returns the core users among ids; unknown and deleted users
// are left out. ok=false: the host cannot tell (users.read not granted),
// and the callers skip what needs it.
func (p *Plugin) lookupUsers(ctx context.Context, ids []int64) (users []*pluginv1.UserInfo, ok bool, err error) {
	if p.host == nil {
		return nil, false, nil
	}
	users, err = pluginsdk.GetUsers(ctx, p.host, ids)
	switch status.Code(err) {
	case codes.OK:
		return users, true, nil
	case codes.PermissionDenied, codes.FailedPrecondition:
		return nil, false, nil
	}
	return nil, false, err
}

// missingUser is the answer to a write naming a user the core does not know
// (404 user_not_found), or nil when the user exists or the host cannot tell.
func (p *Plugin) missingUser(ctx context.Context, uid int64) *pluginv1.HTTPResponse {
	users, ok, err := p.lookupUsers(ctx, []int64{uid})
	if err != nil {
		return pluginsdk.Unavailable("user lookup failed / 查询用户失败: " + err.Error())
	}
	if !ok || len(users) > 0 {
		return nil
	}
	return pluginsdk.ErrorResponse(http.StatusNotFound, "user_not_found", "the user does not exist / 用户不存在")
}

// getUsers: GET /users?ids=1,2 (at most 500) -> the users the core knows
// among those the plugin's moderation tables name: records, the watch list
// and its history (users and operators), bans and unbans. The
// daily statistics (user_daily) do not count: every moderated user has a
// row there, so it would answer for nearly every active user and let a
// moderator list them (CONTRACTS §72.14). 503 users_unavailable when the
// host cannot look users up.
func (p *Plugin) getUsers(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	ids, valid := parseIDList(pluginsdk.Query(req, "ids"))
	if !valid {
		return pluginsdk.FieldErrorResponse("invalid query / 查询参数无效",
			pluginsdk.FieldErrors{}.Add("ids", "invalid", "ids is a comma list of up to 500 user ids / ids 为逗号分隔的用户 id，最多 500 个")), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	known := []int64{}
	err = db.Table("unnest(?::bigint[]) AS u", pgdb.Array[int64](ids)).
		Where(`EXISTS (SELECT 1 FROM events WHERE user_id = u) OR EXISTS (SELECT 1 FROM watchlist WHERE user_id = u)
		OR EXISTS (SELECT 1 FROM watch_events WHERE user_id = u) OR EXISTS (SELECT 1 FROM watch_events WHERE operator_id = u)
		OR EXISTS (SELECT 1 FROM blocks WHERE user_id = u) OR EXISTS (SELECT 1 FROM unblocks WHERE user_id = u)`).
		Pluck("u", &known).Error
	ids = known
	if err != nil {
		return nil, status.Errorf(codes.Internal, "users: %v", err)
	}
	if len(ids) == 0 {
		return pluginsdk.DataResponse([]CoreUser{}), nil
	}
	users, ok, err := p.lookupUsers(ctx, ids)
	if err != nil {
		return pluginsdk.Unavailable("user lookup failed / 查询用户失败: " + err.Error()), nil
	}
	if !ok {
		return pluginsdk.ErrorResponse(http.StatusServiceUnavailable, "users_unavailable",
			"user lookup needs the users.read grant / 查询用户需要 users.read 授权"), nil
	}
	out := make([]CoreUser, len(users))
	for i, u := range users {
		out[i] = CoreUser{ID: u.GetId(), DisplayName: u.GetDisplayName(), Status: u.GetStatus()}
	}
	return pluginsdk.DataResponse(out), nil
}

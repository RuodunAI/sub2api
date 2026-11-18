package volcengine

import (
	"context"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Execute is the one request path. The SDK flow builds the request
// (BuildUpstream) and picks the form from the request's flags: the video
// submission relays, waits and answers the task (ParseTaskSubmission, with
// the refined estimate); every other endpoint relays without waiting and
// answers host_usage.
func (p *Plugin) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
	return pluginsdk.ExecuteDefault(ctx, p, in)
}

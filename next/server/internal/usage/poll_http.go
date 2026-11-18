package usage

import (
	"context"
	"net/http"
	"net/url"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/execfetch"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/netguard"
)

// fetchExecutionHTTP is the fetch of a Monitor call (core.ExecutionHTTP).
// proxyID and admission come from the already bound account, never from the
// plugin; through a proxy the name is the proxy's to resolve
// (netguard.CheckHostVia). It performs one HTTP exchange, without redirects or retries; the
// runtime has already checked the token and the host (neither the number of
// fetches nor their size is bounded).
// in.Options may only shorten reconcileHTTPTimeout.
func (s *Service) fetchExecutionHTTP(ctx context.Context, in *pluginv1.ExecutionHTTPRequest, proxyID *int64, admit func(context.Context) error) (*pluginv1.ExecutionHTTPResponse, error) {
	return execfetch.Do(ctx, in, execfetch.Binding{
		CheckURL: func(ctx context.Context, raw string) (*url.URL, error) {
			return netguard.CheckURLVia(ctx, raw, s.rec.AllowPrivateUpstream, proxyID != nil, netguard.DefaultLookup)
		},
		Client:        func(ctx context.Context) (*http.Client, error) { return s.rec.Proxies.HTTPClient(ctx, proxyID) },
		HeaderTimeout: reconcileHTTPTimeout, TotalTimeout: reconcileHTTPTimeout,
		Admit: admit,
	})
}

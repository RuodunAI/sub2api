package gateway

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/execfetch"
)

// fetchTimeout bounds a fetch when the environment sets no relay timeout:
// its answer is bounded and never streamed to the client.
const fetchTimeout = core.ExecutionHTTPDefaultTimeout

// fetch is ExecutionClient.Fetch: one bounded exchange whose answer goes
// back to the plugin (a token exchange, an upload, a preflight). It is the
// attempt's account's request like the relay - its proxy, the SSRF guard,
// no redirect, the environment's timeouts, which the plugin may only
// shorten - but not the client's request: the operator's passthrough and
// overrides do not apply. The runtime has already checked the token, the
// count, the body size and the host; execfetch holds it to the limits every
// fetch shares with Monitor's. A transport failure is an answer with status
// 0, so the plugin can classify it.
func (x *execution) fetch(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
	c := x.c
	header, total := c.upstreamTimeouts(nil, false)
	out, err := execfetch.Do(ctx, in, execfetch.Binding{
		CheckURL:      c.g.checkUpstreamURL,
		Client:        func(ctx context.Context) (*http.Client, error) { return c.g.d.Proxies.HTTPClient(ctx, x.acc.ProxyID) },
		HeaderTimeout: header, TotalTimeout: cmp.Or(total, fetchTimeout),
	})
	if err == nil && out.GetTransportError() == "" {
		u, _ := url.Parse(in.GetUrl())
		slog.InfoContext(ctx, "gateway: plugin fetch", "request_id", c.rid, "plugin", x.rt.binding.Plugin.Key, "account", x.acc.ID,
			"method", cmp.Or(strings.ToUpper(in.GetMethod()), http.MethodGet), "host", u.Host, "path", u.EscapedPath(), "status", out.GetStatus(), "bytes", len(out.GetBody()))
	}
	return out, err
}

// noRedirects is a copy of client that returns a redirect as the answer,
// without a client-wide timeout: the exchange's own timers bound it.
func noRedirects(client *http.Client) *http.Client {
	hc := *client
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Timeout = 0
	return &hc
}

// firstHeaderValues is h as a map of lower-case names to their first value,
// the form plugins receive headers in.
func firstHeaderValues(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[strings.ToLower(k)] = v[0]
		}
	}
	return out
}

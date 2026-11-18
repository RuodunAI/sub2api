package gateway

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// fetchTimeout bounds a fetch when the environment sets no relay timeout:
// its answer is bounded and never streamed to the client.
const fetchTimeout = 5 * time.Minute

// fetch is ExecutionClient.Fetch: one bounded exchange whose answer goes
// back to the plugin (a token exchange, an upload, a preflight). It is the
// attempt's account's request like the relay - its proxy, the SSRF guard,
// no redirect, the environment's timeouts, which the plugin may only
// shorten - but not the client's request: the operator's passthrough and
// overrides do not apply. The runtime has already checked the token, the
// count, the body size and the host. A transport failure is an answer with
// status 0, so the plugin can classify it.
func (x *execution) fetch(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
	c := x.c
	failed := func(msg string) (*pluginv1.ExecutionHTTPResponse, error) {
		return &pluginv1.ExecutionHTTPResponse{TransportError: msg}, nil
	}
	method := cmp.Or(strings.ToUpper(in.GetMethod()), http.MethodGet)
	if e := checkRelayHeaders(&pluginv1.UpstreamRequest{Method: method, Headers: in.GetHeaders()}); e != nil {
		return failed(e.Message)
	}
	target, err := c.g.checkUpstreamURL(ctx, in.GetUrl())
	if err != nil {
		return failed("fetch url rejected by host policy")
	}
	client, err := c.g.d.Proxies.HTTPClient(ctx, x.acc.ProxyID)
	if err != nil || client == nil {
		return failed("account proxy unavailable")
	}
	header, total := c.upstreamTimeouts(in.GetOptions(), false)
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(total, fetchTimeout))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(in.GetBody()))
	if err != nil {
		return failed("invalid fetch request")
	}
	setBuiltHeaders(req, in.GetHeaders())
	if len(in.GetBody()) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := noRedirects(client)
	timer := time.AfterFunc(header, cancel)
	resp, err := hc.Do(req)
	timer.Stop()
	if err != nil {
		return failed("fetch failed: " + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, core.ExecutionHTTPMaxResponseBytes+1))
	out := &pluginv1.ExecutionHTTPResponse{Status: int32(resp.StatusCode), Headers: firstHeaderValues(resp.Header), Body: raw}
	if len(raw) > core.ExecutionHTTPMaxResponseBytes {
		out.Body, out.Truncated = raw[:core.ExecutionHTTPMaxResponseBytes], true
	}
	if readErr != nil {
		out.Truncated, out.TransportError = true, "incomplete fetch response: "+readErr.Error()
	}
	slog.InfoContext(ctx, "gateway: plugin fetch", "request_id", c.rid, "plugin", x.rt.binding.Plugin.Key, "account", x.acc.ID,
		"method", method, "host", target.Host, "path", target.EscapedPath(), "status", resp.StatusCode, "bytes", len(out.Body))
	return out, nil
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

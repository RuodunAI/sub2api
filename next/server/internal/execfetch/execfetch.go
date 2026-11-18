// Package execfetch is the fetch form of plugin execution
// (core.ExecutionHTTP): one bounded HTTP exchange whose answer goes back to
// the plugin. The gateway serves the fetches of Execute and the usage module
// those of Monitor through this one implementation, each with its own
// binding (the account's client, the SSRF policy, its base timeouts, an
// admission), so the request and answer limits cannot drift apart.
package execfetch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// Limits of a fetch request and of the headers of its answer.
const (
	MaxURLBytes    = 8192
	MaxHeaders     = 64
	MaxHeaderBytes = 32 << 10
)

// TransportFailure is the plugin's text for an exchange that failed in
// transport; the cause, which may name the proxy, stays in the host's log.
const TransportFailure = "execution HTTP request failed"

// Binding is what the host binds a fetch to; none of it comes from the
// plugin.
type Binding struct {
	// CheckURL applies the SSRF policy (netguard) to the request's URL.
	CheckURL func(ctx context.Context, raw string) (*url.URL, error)
	// Client is the bound account's HTTP client (its proxy). The fetch uses
	// a copy that follows no redirect and has no client-wide timeout.
	Client func(ctx context.Context) (*http.Client, error)
	// HeaderTimeout and TotalTimeout are the host's; the request's options
	// may only shorten them. <= 0 is none.
	HeaderTimeout, TotalTimeout time.Duration
	// Admit, when set, is asked right before the exchange; its error is
	// returned as the fetch's error and nothing is sent.
	Admit func(context.Context) error
}

// Do performs one fetch: no redirects, no retries, the answer body whole,
// its headers capped at MaxHeaders and MaxHeaderBytes (Truncated set when
// cut). A request the host refuses or an
// exchange that fails is an answer with TransportError set; an error is the
// context's or Admit's.
func Do(ctx context.Context, in *pluginv1.ExecutionHTTPRequest, b Binding) (*pluginv1.ExecutionHTTPResponse, error) {
	failure := func(msg string) (*pluginv1.ExecutionHTTPResponse, error) {
		return &pluginv1.ExecutionHTTPResponse{TransportError: msg}, nil
	}
	method, msg := checkRequest(in)
	if msg != "" {
		return failure(msg)
	}
	opts := in.GetOptions()
	if total := Tighten(b.TotalTimeout, opts.GetTotalTimeoutMs()); total > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u, err := b.CheckURL(ctx, in.GetUrl())
	if err != nil {
		return failure("execution URL rejected by host policy")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(in.GetBody()))
	if err != nil {
		return failure("invalid execution HTTP request")
	}
	for k, v := range in.GetHeaders() {
		if !strings.EqualFold(k, "accept-encoding") { // the transport negotiates and decodes it
			req.Header.Set(k, v)
		}
	}
	if len(in.GetBody()) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	hc, err := b.Client(ctx)
	if err != nil || hc == nil {
		return failure("execution account proxy is unavailable")
	}
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 0
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if b.Admit != nil {
		if err := b.Admit(ctx); err != nil {
			return nil, err
		}
	}
	// A response header timeout cuts the exchange when no answer started in
	// time; it is the total timeout's to bound the rest.
	stop := func() bool { return false }
	if header := Tighten(b.HeaderTimeout, opts.GetResponseHeaderTimeoutMs()); header > 0 {
		stop = time.AfterFunc(header, cancel).Stop
	}
	resp, err := client.Do(req)
	stop()
	return answer(ctx, u, resp, err)
}

// checkRequest is the request's method, or why the host refuses it.
func checkRequest(in *pluginv1.ExecutionHTTPRequest) (method, refused string) {
	if in == nil || len(in.GetUrl()) > MaxURLBytes || len(in.GetHeaders()) > MaxHeaders {
		return "", "execution request exceeds host limits"
	}
	method = strings.ToUpper(strings.TrimSpace(in.GetMethod()))
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return "", "execution HTTP method is not allowed"
	}
	size := 0
	for k, v := range in.GetHeaders() {
		size += len(k) + len(v)
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) || size > MaxHeaderBytes {
			return "", "invalid execution request headers"
		}
		switch strings.ToLower(k) {
		case "host", "connection", "proxy-connection", "proxy-authorization", "proxy-authenticate", "upgrade", "transfer-encoding", "trailer", "te", "content-length":
			return "", "execution request contains a forbidden transport header"
		}
	}
	return method, ""
}

// answer reads the exchange's answer.
func answer(ctx context.Context, u *url.URL, resp *http.Response, err error) (*pluginv1.ExecutionHTTPResponse, error) {
	if err != nil {
		slog.InfoContext(ctx, "execfetch: exchange failed", "host", u.Host, "err", err)
		return &pluginv1.ExecutionHTTPResponse{TransportError: TransportFailure}, nil
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	out := &pluginv1.ExecutionHTTPResponse{Status: int32(resp.StatusCode), Headers: map[string]string{}, Body: raw}
	size := 0
	for k, v := range resp.Header {
		if len(v) == 0 {
			continue
		}
		size += len(k) + len(v[0])
		if len(out.Headers) >= MaxHeaders || size > MaxHeaderBytes {
			out.Truncated = true
			break
		}
		out.Headers[strings.ToLower(k)] = v[0]
	}
	if readErr != nil {
		out.TransportError = "incomplete execution HTTP response"
	}
	return out, nil
}

// Tighten is d, or ms milliseconds when that is shorter (or d is none);
// ms <= 0 asks nothing.
func Tighten(d time.Duration, ms int32) time.Duration {
	if t := time.Duration(ms) * time.Millisecond; ms > 0 && (d <= 0 || t < d) {
		return t
	}
	return d
}

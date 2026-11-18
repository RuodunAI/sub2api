package usage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/netguard"
	"golang.org/x/net/http/httpguts"
)

// fetchExecutionHTTP is the fetch of a Monitor call (core.ExecutionHTTP).
// proxyID and admission come from the already bound account, never from the
// plugin. It performs one HTTP exchange, without redirects or retries; the
// runtime has already checked the count, the request body and the host.
// in.Options may only shorten reconcileHTTPTimeout.
func (s *Service) fetchExecutionHTTP(ctx context.Context, in *pluginv1.ExecutionHTTPRequest, proxyID *int64, admit func(context.Context) error) (*pluginv1.ExecutionHTTPResponse, error) {
	failure := func(msg string) (*pluginv1.ExecutionHTTPResponse, error) {
		return &pluginv1.ExecutionHTTPResponse{TransportError: msg}, nil
	}
	if in == nil || len(in.GetUrl()) > 8192 || len(in.GetHeaders()) > 64 {
		return failure("execution request exceeds host limits")
	}
	method := strings.ToUpper(strings.TrimSpace(in.GetMethod()))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		return failure("execution HTTP method is not allowed")
	}
	headerBytes := 0
	for k, v := range in.GetHeaders() {
		headerBytes += len(k) + len(v)
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) || headerBytes > 32<<10 {
			return failure("invalid execution request headers")
		}
		switch strings.ToLower(k) {
		case "host", "connection", "proxy-connection", "proxy-authorization", "proxy-authenticate", "upgrade", "transfer-encoding", "trailer", "te", "content-length":
			return failure("execution request contains a forbidden transport header")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, shorter(reconcileHTTPTimeout, in.GetOptions().GetTotalTimeoutMs()))
	defer cancel()
	u, err := netguard.CheckURL(ctx, in.GetUrl(), s.rec.AllowPrivateUpstream, netguard.DefaultLookup)
	if err != nil {
		return failure("execution URL rejected by host policy")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(in.GetBody()))
	if err != nil {
		return failure("invalid execution HTTP request")
	}
	for k, v := range in.GetHeaders() {
		req.Header.Set(k, v)
	}
	if len(in.GetBody()) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	hc, err := s.rec.Proxies.HTTPClient(ctx, proxyID)
	if err != nil || hc == nil {
		return failure("execution account proxy is unavailable")
	}
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if admit != nil {
		if err := admit(ctx); err != nil {
			return nil, err
		}
	}
	// A response header timeout cuts the exchange when no answer started in
	// time; it is the total timeout's to bound the rest.
	headers := time.AfterFunc(shorter(reconcileHTTPTimeout, in.GetOptions().GetResponseHeaderTimeoutMs()), cancel)
	resp, err := client.Do(req)
	headers.Stop()
	if err != nil {
		return failure("execution HTTP request failed")
	}
	defer resp.Body.Close()
	const limit = core.ExecutionHTTPMaxResponseBytes
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	truncated := len(raw) > limit || readErr != nil
	if len(raw) > limit {
		raw = raw[:limit]
	}
	out := &pluginv1.ExecutionHTTPResponse{Status: int32(resp.StatusCode), Headers: map[string]string{}, Body: raw, Truncated: truncated}
	headerBytes = 0
	for k, v := range resp.Header {
		if len(v) > 0 {
			headerBytes += len(k) + len(v[0])
			if len(out.Headers) >= 64 || headerBytes > 32<<10 {
				out.Truncated = true
				break
			}
			out.Headers[strings.ToLower(k)] = v[0]
		}
	}
	if readErr != nil {
		out.TransportError = "incomplete execution HTTP response"
	}
	return out, nil
}

// shorter is d, or ms milliseconds when that is positive and shorter.
func shorter(d time.Duration, ms int32) time.Duration {
	if ms > 0 {
		return min(d, time.Duration(ms)*time.Millisecond)
	}
	return d
}

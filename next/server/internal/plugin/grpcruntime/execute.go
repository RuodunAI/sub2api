package grpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// transportHeaders are the request headers the host's transport decides; a
// plugin's request naming one is refused.
var transportHeaders = map[string]bool{"content-length": true, "accept-encoding": true, "connection": true, "transfer-encoding": true}

// maxRequestBody is the request body limit of the moment.
func (h *hostServer) maxRequestBody() int64 {
	return core.MaxRequestBodyBytes(h.i.rt.o.RequestBodyLimit)
}

// relayBodyMargin is what a relayed body may exceed its bound by: room for
// the plugin's envelope and added fields.
const relayBodyMargin = 1 << 20

// checkBody refuses a body above limit bytes. The broker accepts messages of
// any size (the setting can change while it runs), so every handler taking a
// body checks it here.
func checkBody(body []byte, limit int64) error {
	if int64(len(body)) > limit {
		return status.Errorf(codes.InvalidArgument, "body of %d bytes exceeds the limit of %d bytes", len(body), limit)
	}
	return nil
}

// relayBodyLimit bounds the body of a relay: the larger of the request body
// limit and the body this execution handed to the plugin, plus a margin.
// The core may have enlarged the client's body after admitting it (inlined
// media, hook patches, parameter overrides); what the plugin got it must be
// able to hand back.
func (h *hostServer) relayBodyLimit(sc *callScope) int64 {
	return max(h.maxRequestBody(), sc.bodyBytes) + relayBodyMargin
}

// checkUpstreamRequest is the part of validating a relay that needs nothing
// of the gateway: the request is there, within the body bound, and leaves
// the transport's headers alone.
func (h *hostServer) checkUpstreamRequest(sc *callScope, req *pluginv1.UpstreamRequest) error {
	if req == nil || req.GetUrl() == "" {
		return status.Error(codes.InvalidArgument, "request with a url is required")
	}
	if err := checkBody(req.GetBody(), h.relayBodyLimit(sc)); err != nil {
		return err
	}
	for _, headers := range []map[string]string{req.GetHeaders(), req.GetDefaultHeaders()} {
		for name := range headers {
			if transportHeaders[strings.ToLower(name)] {
				return status.Errorf(codes.InvalidArgument, "header %q is set by the host's transport", name)
			}
		}
	}
	return nil
}

// RelayUpstream takes the final upstream request of an execution. Without
// wait it is only checked and kept: platformAdapter.Execute hands it to the
// gateway once the plugin returned host_usage, so nothing is sent while the
// plugin is still in its call. With wait the gateway sends and relays now
// and the plugin gets the observation.
func (h *hostServer) RelayUpstream(ctx context.Context, in *pluginv1.RelayUpstreamRequest) (out *pluginv1.RelayUpstreamResponse, err error) {
	sc, err := h.enter(in.GetExecutionToken())
	if err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			out, err = nil, status.Error(codes.Internal, "relay failed internally")
		}
		sc.leave()
	}()
	switch {
	case !sc.relay:
		return nil, status.Error(codes.PermissionDenied, "RelayUpstream is for Execute calls")
	case sc.relayed:
		return nil, status.Error(codes.FailedPrecondition, "upstream already relayed")
	case in.GetWait() && (!sc.wait || sc.client.Relay == nil):
		return nil, status.Error(codes.FailedPrecondition, "a waiting relay needs extract_usage or task_submission")
	}
	req := in.GetRequest()
	if err := h.checkUpstreamRequest(sc, req); err != nil {
		return nil, err
	}
	cctx, cancel := sc.hostContext(ctx)
	defer cancel()
	if sc.client.Check != nil {
		if err := sc.client.Check(cctx, req); err != nil {
			return nil, status.Error(codes.InvalidArgument, refusal(err))
		}
	}
	sc.relayed = true
	if !in.GetWait() {
		sc.parked = req
		return &pluginv1.RelayUpstreamResponse{}, nil
	}
	out, err = sc.client.Relay(cctx, req)
	if err != nil {
		return nil, status.Error(codes.Unavailable, refusal(err))
	}
	if (out.GetObservation() == nil) == (out.GetError() == nil) {
		return nil, status.Error(codes.Internal, "relay ended without exactly one of observation and error")
	}
	return out, nil
}

// refusal is the text of a host refusal for the plugin: the message of a
// core error, without its internal cause.
func refusal(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Message
	}
	return err.Error()
}

// ExecuteHTTP is the fetch form: one bounded exchange for the plugin, during
// an Execute or a Monitor call.
func (h *hostServer) ExecuteHTTP(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (out *pluginv1.ExecutionHTTPResponse, err error) {
	sc, err := h.enter(in.GetExecutionToken())
	if err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			out, err = nil, status.Error(codes.Internal, "execution HTTP failed internally")
		}
		sc.leave()
	}()
	if sc.client.Fetch == nil {
		return nil, status.Error(codes.FailedPrecondition, "this call has no fetch capability")
	}
	if sc.fetches >= core.ExecutionHTTPMaxRequests {
		return nil, status.Errorf(codes.ResourceExhausted, "at most %d requests per execution", core.ExecutionHTTPMaxRequests)
	}
	if err := checkBody(in.GetBody(), h.maxRequestBody()); err != nil {
		return nil, err
	}
	if err := h.requireFetchHost(sc, in.GetUrl()); err != nil {
		return nil, err
	}
	sc.fetches++
	cctx, cancel := sc.hostContext(ctx)
	defer cancel()
	out, err = sc.client.Fetch(cctx, in)
	if err != nil {
		return nil, toStatus(err)
	}
	if out == nil {
		return nil, status.Error(codes.Internal, "fetch returned no response")
	}
	if len(out.Body) > core.ExecutionHTTPMaxResponseBytes {
		out.Body, out.Truncated = out.Body[:core.ExecutionHTTPMaxResponseBytes], true
	}
	if cctx.Err() == nil && out.GetStatus() >= 200 && out.GetStatus() <= 599 && out.GetTransportError() == "" && !out.GetTruncated() {
		sc.evidence = true
	}
	return out, nil
}

// requireFetchHost accepts the http(s) URL of a fetch whose host is one of
// the manifest's externalServices (or a subdomain of one), which the
// operator reviewed at install, or the host of one of the account's own
// upstream addresses, which the operator entered: an account pointed at a
// relay must be able to query it. Whether the address may be reached at all
// (the SSRF policy, the account's proxy) is the fetch's to decide, not this
// check's.
func (h *hostServer) requireFetchHost(sc *callScope, raw string) error {
	host, err := httpHost(raw)
	if err != nil {
		return err
	}
	if !h.externalService(host) && !slices.Contains(h.i.accountHosts(sc.account), host) {
		return status.Errorf(codes.PermissionDenied, "plugin %s: url must be http(s) on a host of the manifest's externalServices or of the account's upstream addresses", h.key())
	}
	return nil
}

// accountHosts are the lower-case host names of the account's own upstream
// addresses: the values of the address fields its type declares (manifest
// upstreamFields), wherever the account keeps them.
func (i *Instance) accountHosts(acct *pluginv1.Account) []string {
	at := slices.IndexFunc(i.pkg.Manifest.AccountTypes, func(at manifest.AccountType) bool { return at.ID == acct.GetType() })
	if at < 0 {
		return nil
	}
	fields, _ := i.pkg.Manifest.AccountTypes[at].AddressFields()
	var hosts []string
	for _, doc := range []string{acct.GetSettingsJson(), acct.GetCredentialsJson()} {
		var values map[string]any
		if len(fields) == 0 || json.Unmarshal([]byte(doc), &values) != nil {
			continue
		}
		for _, f := range fields {
			address, _ := values[f].(string)
			if host := addressHost(address); host != "" {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

// addressHost is the lower-case host name of an upstream address, which is
// a URL or a bare host[:port]; "" when it names none.
func addressHost(address string) string {
	address = strings.TrimSpace(address)
	if !strings.Contains(address, "://") {
		address = "//" + address
	}
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Execute runs PlatformService.Execute for one attempt (core.ExecutePlugin).
func (a platformAdapter) Execute(ctx context.Context, in *pluginv1.ExecuteRequest, client core.ExecutionClient) (res core.ExecutionResult, err error) {
	if in == nil {
		return res, errors.New("Execute requires a request")
	}
	segment := client.SegmentTimeout
	if segment <= 0 {
		segment = TimeoutPlatformHot
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Execute may span a whole upstream exchange (a relay that waits), so
	// it has its own class and no overall timeout: long streams never hold
	// the slots of the request-path RPCs (CONTRACTS §43.1). What is bounded
	// is each stretch of the plugin's own work.
	err = a.i.call(ctx, classExecute, 0, func(ctx context.Context, p *proc) error {
		sc := &callScope{ctx: ctx, client: client, account: in.GetAccount(), relay: true, wait: in.GetExtractUsage() || in.GetTaskSubmission(),
			bodyBytes: int64(len(in.GetBody())), segmentTimeout: segment}
		sc.segment = time.AfterFunc(segment, func() { cancel(errSegmentTimeout) })
		token, err := p.scopes.open(sc)
		if err != nil {
			sc.segment.Stop()
			return err
		}
		req := shallowClone(in)
		req.ExecutionToken = token
		out, err := p.platform.Execute(ctx, req)
		// Whatever the plugin left running ends here: a host call still in
		// progress is cancelled and joined before the result is read.
		cancel(nil)
		p.scopes.close(token, sc)
		if err != nil {
			return err
		}
		if out == nil {
			return errors.New("Execute returned no response")
		}
		res.Response = out
		// A parked request leaves the runtime only with host_usage: any
		// other answer, like any error above, leaves it unsent.
		if out.GetHostUsage() != nil {
			res.Relay = sc.parked
		}
		return nil
	})
	if errors.Is(context.Cause(ctx), errSegmentTimeout) {
		return core.ExecutionResult{}, a.i.unavailableFor(core.PluginUnavailableSegmentTimeout, fmt.Errorf("%w (%s)", errSegmentTimeout, segment))
	}
	return res, err
}

// unavailableFor is unavailable with the reason in the error's details, for
// failures the gateway answers by moving to another account.
func (i *Instance) unavailableFor(reason string, cause error) error {
	return core.ErrPluginUnavailable.WithDetails(map[string]any{"plugin_key": i.pkg.Key, "reason": reason}).
		WithCause(fmt.Errorf("plugin %s@%s: %w", i.pkg.Key, i.pkg.Version, cause))
}

var _ core.ExecutePlugin = platformAdapter{}

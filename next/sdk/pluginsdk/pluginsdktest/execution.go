package pluginsdktest

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// RelayCall is one relay the fake host accepted.
type RelayCall struct {
	Request *pluginv1.UpstreamRequest
	// Wait is the form: false is "validate now, send after Execute".
	Wait bool
}

// Execution is one Execute or Monitor call as the host sees it: the token
// it issued, what the plugin did with the host's HTTP client under that
// token and, once the call returned, its outcome. Harness.Execute and
// Harness.Monitor return it finished; FakeHost.Begin starts one by hand.
type Execution struct {
	// Response and Err are the outcome of an Execute call, MonitorResponse
	// and Err of a Monitor call.
	Response        *pluginv1.ExecuteResponse
	MonitorResponse *pluginv1.MonitorResponse
	Err             error

	host      *FakeHost
	token     string
	request   *pluginv1.ExecuteRequest // nil for Monitor
	ended     bool
	relays    []RelayCall
	relayErrs []error
	fetches   []*pluginv1.ExecutionHTTPRequest
}

// Begin issues the token of one Execute call (in non-nil; its two flags
// decide whether a relay may wait) or one Monitor call (in nil). The token
// is live until End.
func (f *FakeHost) Begin(in *pluginv1.ExecuteRequest) *Execution {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.executions == nil {
		f.executions = map[string]*Execution{}
	}
	e := &Execution{host: f, token: fmt.Sprintf("execution-%d", len(f.executions)+1), request: in}
	f.executions[e.token] = e
	return e
}

// Token is the execution token of the call.
func (e *Execution) Token() string { return e.token }

// End voids the token, as the host does when the call returns: a relay or
// fetch made with it afterwards is PERMISSION_DENIED.
func (e *Execution) End() {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	e.ended = true
}

// ExpireExecutions voids the token of every call in flight, e.g. from a
// RelayFunc or ExecuteHTTPFunc, to see how a plugin copes with a host that
// has given up on the call.
func (f *FakeHost) ExpireExecutions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.executions {
		e.ended = true
	}
}

// Relays returns the relays the host accepted, in order. A conforming
// Execute makes exactly one (see Relayed).
func (e *Execution) Relays() []RelayCall {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	return append([]RelayCall(nil), e.relays...)
}

// RelayErrors returns the errors the host answered relays with: a malformed
// request, a second relay, a wait the flags do not allow, or what RelayFunc
// scripted.
func (e *Execution) RelayErrors() []error {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	return append([]error(nil), e.relayErrs...)
}

// Fetches returns the fetches the host accepted, in order.
func (e *Execution) Fetches() []*pluginv1.ExecutionHTTPRequest {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	return append([]*pluginv1.ExecutionHTTPRequest(nil), e.fetches...)
}

// Relayed returns the one relay of the execution and fails the test unless
// the call succeeded and relayed exactly once, with no relay the host
// answered with an error.
func (e *Execution) Relayed(t testing.TB) RelayCall {
	t.Helper()
	relays, failed := e.Relays(), e.RelayErrors()
	switch {
	case e.Err != nil:
		t.Fatalf("pluginsdktest: the call failed: %v", e.Err)
	case len(relays) != 1 || len(failed) != 0:
		t.Fatalf("pluginsdktest: want exactly one relay, got %d accepted and %d failed %v", len(relays), len(failed), failed)
	default:
		return relays[0]
	}
	return RelayCall{}
}

// Observed is the answer of a waited relay to a 2xx: the bounded
// observation with the whole body of a non-streaming answer. Set events,
// headers or truncated on the returned observation for other cases. The
// fake fills meta and the account (without credentials) from the request.
func Observed(status int32, body []byte) *pluginv1.RelayUpstreamResponse {
	return &pluginv1.RelayUpstreamResponse{Observation: &pluginv1.ExtractUsageRequest{Status: status, Body: body}}
}

// UpstreamFailed is the answer of a waited relay to a failed exchange: a
// non-2xx status with the start of its body, or status 0 with
// transportError.
func UpstreamFailed(status int32, bodyPrefix []byte, transportError string) *pluginv1.RelayUpstreamResponse {
	return &pluginv1.RelayUpstreamResponse{Error: &pluginv1.ClassifyErrorRequest{Status: status, BodyPrefix: bodyPrefix, TransportError: transportError}}
}

// live returns the execution of token while its call is in flight.
func (f *FakeHost) live(token string) (*Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.executions[token]
	if e == nil || e.ended {
		return nil, status.Error(codes.PermissionDenied, "execution token is not that of a call in flight")
	}
	return e, nil
}

func (f *FakeHost) RelayUpstream(ctx context.Context, in *pluginv1.RelayUpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
	e, err := f.live(in.GetExecutionToken())
	if err != nil {
		return nil, err
	}
	out, err := f.relay(ctx, e, in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		e.relayErrs = append(e.relayErrs, err)
		return nil, err
	}
	return out, nil
}

func (f *FakeHost) relay(ctx context.Context, e *Execution, in *pluginv1.RelayUpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
	if err := f.admitRelay(e, in); err != nil {
		return nil, err
	}
	var out *pluginv1.RelayUpstreamResponse
	if f.RelayFunc != nil {
		var err error
		if out, err = f.RelayFunc(ctx, in.GetRequest()); status.Code(err) == codes.InvalidArgument {
			f.mu.Lock()
			e.relays = e.relays[:len(e.relays)-1] // refused: not relayed
			f.mu.Unlock()
			return nil, err
		} else if err != nil {
			return nil, err
		}
	}
	if !in.GetWait() {
		return &pluginv1.RelayUpstreamResponse{}, nil
	}
	if f.RelayFunc == nil {
		return nil, status.Error(codes.Unimplemented, "set FakeHost.RelayFunc to answer a relay that waits")
	}
	if o := out.GetObservation(); o != nil {
		if o.Meta == nil {
			o.Meta = e.request.GetMeta()
		}
		if o.Account == nil && e.request.GetAccount() != nil {
			o.Account = proto.Clone(e.request.GetAccount()).(*pluginv1.Account)
			o.Account.CredentialsJson = ""
		}
	}
	return out, nil
}

// admitRelay applies the host's checks of a relay and records it.
func (f *FakeHost) admitRelay(e *Execution, in *pluginv1.RelayUpstreamRequest) error {
	if err := pluginsdk.ValidateUpstreamRequest(in.GetRequest()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if f.MaxBodyBytes > 0 && len(in.GetRequest().GetBody()) > f.MaxBodyBytes {
		return status.Error(codes.InvalidArgument, "upstream body exceeds the request body limit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case e.request == nil:
		return status.Error(codes.FailedPrecondition, "only Execute relays, this is a Monitor call")
	case in.GetWait() && !e.request.GetExtractUsage() && !e.request.GetTaskSubmission():
		return status.Error(codes.FailedPrecondition, "a relay may wait only when the execution sets extract_usage or task_submission")
	case len(e.relays) > 0:
		return status.Error(codes.FailedPrecondition, "the execution has already relayed")
	}
	e.relays = append(e.relays, RelayCall{Request: in.GetRequest(), Wait: in.GetWait()})
	return nil
}

func (f *FakeHost) ExecuteHTTP(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
	e, err := f.live(in.GetExecutionToken())
	if err != nil {
		return nil, err
	}
	if err := f.admitFetch(e, in); err != nil {
		return nil, err
	}
	if f.ExecuteHTTPFunc == nil {
		return nil, status.Error(codes.Unimplemented, "set FakeHost.ExecuteHTTPFunc to answer a fetch")
	}
	out, err := f.ExecuteHTTPFunc(ctx, in)
	if err == nil && len(out.GetBody()) > pluginsdk.FetchResponseLimit {
		out.Body, out.Truncated = out.Body[:pluginsdk.FetchResponseLimit], true
	}
	return out, err
}

// admitFetch applies the host's checks of a fetch and records it.
func (f *FakeHost) admitFetch(e *Execution, in *pluginv1.ExecutionHTTPRequest) error {
	u, err := url.Parse(in.GetUrl())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return status.Errorf(codes.InvalidArgument, "fetch url %q is not an absolute http(s) URL", in.GetUrl())
	}
	if f.ExternalServices != nil && !hostAllowed(u.Hostname(), f.ExternalServices) {
		return status.Errorf(codes.PermissionDenied, "fetch host %q is not one of the manifest's externalServices", u.Hostname())
	}
	if f.MaxBodyBytes > 0 && len(in.GetBody()) > f.MaxBodyBytes {
		return status.Error(codes.InvalidArgument, "fetch body exceeds the request body limit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(e.fetches) >= pluginsdk.FetchLimit {
		return status.Errorf(codes.ResourceExhausted, "an execution fetches at most %d times", pluginsdk.FetchLimit)
	}
	e.fetches = append(e.fetches, in)
	return nil
}

func hostAllowed(host string, services []string) bool {
	host = strings.ToLower(host)
	for _, s := range services {
		s = strings.ToLower(s)
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// largeMessages lets the harness carry the answers the host's client may
// return (a fetch answer is up to pluginsdk.FetchResponseLimit).
var largeMessages = grpc.MaxCallRecvMsgSize(math.MaxInt32)

// Execute makes one PlatformService.Execute call the way the host does: it
// issues the execution token (one set on in is replaced), calls the plugin
// and voids the token when the call returns. in is not changed. The
// returned Execution holds the outcome and what the plugin relayed and
// fetched; it does not fail the test, so a test can assert errors too.
func (h *Harness) Execute(ctx context.Context, in *pluginv1.ExecuteRequest) *Execution {
	in = proto.Clone(in).(*pluginv1.ExecuteRequest)
	e := h.Host.Begin(in)
	defer e.End()
	in.ExecutionToken = e.token
	e.Response, e.Err = h.Platform.Execute(ctx, in, largeMessages)
	return e
}

// Monitor makes one PlatformService.Monitor call the way the host does; see
// Execute.
func (h *Harness) Monitor(ctx context.Context, in *pluginv1.PollRequest) *Execution {
	in = proto.Clone(in).(*pluginv1.PollRequest)
	e := h.Host.Begin(nil)
	defer e.End()
	in.ExecutionToken = e.token
	e.MonitorResponse, e.Err = h.Platform.Monitor(ctx, in, largeMessages)
	return e
}

package grpcruntime

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/netguard"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry"
)

const mib = 1 << 20

// pluginSide is the plugin's part of one call in these tests: it gets the
// host server the real plugin would reach over the broker.
type pluginSide struct {
	h       *hostServer
	execute func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error)
	monitor func(ctx context.Context, h *hostServer, in *pluginv1.PollRequest) (*pluginv1.MonitorResponse, error)
}

// executionInstance is an instance whose process is played by side. Its
// manifest names api.example.com as its only external service.
func executionInstance(t *testing.T, side *pluginSide) (*Instance, platformAdapter) {
	t.Helper()
	i, _ := testInstance(t, nil, []string{manifest.CapPlatformAdapter, manifest.CapPlatformMonitor}, registry.Grants{})
	i.pkg.Manifest.ExternalServices = []string{"api.example.com"}
	p := &proc{limits: limitsOf(i.launchSpec())}
	side.h = &hostServer{i: i, p: p}
	p.platform = &platformRPC{
		execute: func(ctx context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
			return side.execute(ctx, side.h, in)
		},
		monitor: func(ctx context.Context, in *pluginv1.PollRequest) (*pluginv1.MonitorResponse, error) {
			return side.monitor(ctx, side.h, in)
		},
	}
	i.proc.Store(p)
	return i, platformAdapter{i}
}

var (
	hostUsage = &pluginv1.ExecuteResponse{Report: &pluginv1.ExecuteResponse_HostUsage{HostUsage: &pluginv1.HostUsage{}}}
	usage     = &pluginv1.ExecuteResponse{Report: &pluginv1.ExecuteResponse_Usage{Usage: &pluginv1.UsageReport{}}}
)

func relay(ctx context.Context, h *hostServer, token string, wait bool, body int) (*pluginv1.RelayUpstreamResponse, error) {
	return h.RelayUpstream(ctx, &pluginv1.RelayUpstreamRequest{ExecutionToken: token, Wait: wait,
		Request: &pluginv1.UpstreamRequest{Url: "https://upstream.example.com/v1", Body: make([]byte, body)}})
}

func fetch(ctx context.Context, h *hostServer, token, url string, body int) (*pluginv1.ExecutionHTTPResponse, error) {
	return h.ExecuteHTTP(ctx, &pluginv1.ExecutionHTTPRequest{ExecutionToken: token, Url: url, Body: make([]byte, body)})
}

func ok200(context.Context, *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
	return &pluginv1.ExecutionHTTPResponse{Status: 200}, nil
}

// reason is the reason detail of a plugin-unavailable error.
func reason(err error) any {
	if err == nil {
		return nil
	}
	return core.AsError(err).Details["reason"]
}

// A relay that does not wait is checked and kept, nothing more: the request
// leaves the runtime only when Execute has returned, and only with
// host_usage.
func TestRelayWithoutWaitSendsNothingBeforeExecuteReturns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func() (*pluginv1.ExecuteResponse, error)
		relay  bool
	}{
		{"host_usage", func() (*pluginv1.ExecuteResponse, error) { return hostUsage, nil }, true},
		{"usage", func() (*pluginv1.ExecuteResponse, error) { return usage, nil }, false},
		{"nothing stated", func() (*pluginv1.ExecuteResponse, error) { return &pluginv1.ExecuteResponse{}, nil }, false},
		{"error", func() (*pluginv1.ExecuteResponse, error) { return nil, status.Error(codes.Internal, "boom") }, false},
		{"crash", func() (*pluginv1.ExecuteResponse, error) {
			return nil, status.Error(codes.Unavailable, "transport is closing")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var events []string
			note := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
			side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
				out, err := relay(ctx, h, in.GetExecutionToken(), false, 10)
				if err != nil || out.GetObservation() != nil || out.GetError() != nil {
					t.Errorf("relay: %v %v", out, err)
				}
				if _, err := relay(ctx, h, in.GetExecutionToken(), false, 10); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("second relay: %v", err)
				}
				time.Sleep(30 * time.Millisecond)
				note("plugin returns")
				return tc.answer()
			}}
			_, a := executionInstance(t, side)
			res, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{}, core.ExecutionClient{
				Check: func(context.Context, *pluginv1.UpstreamRequest) error { note("check"); return nil },
				Relay: func(context.Context, *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
					note("send")
					return nil, nil
				},
			})
			note("execute returned")
			if !slices.Equal(events, []string{"check", "plugin returns", "execute returned"}) {
				t.Fatalf("events %v", events)
			}
			if (res.Relay != nil) != tc.relay || (tc.relay && (err != nil || res.Relay.GetUrl() != "https://upstream.example.com/v1" || len(res.Relay.GetBody()) != 10)) {
				t.Fatalf("relay %v err %v", res.Relay, err)
			}
			if tc.name == "crash" && core.AsError(err).Code != core.ErrPluginUnavailable.Code {
				t.Fatalf("crash: %v", err)
			}
		})
	}
}

// A request the gateway's check refuses is INVALID_ARGUMENT and does not
// count as the execution's relay.
func TestRelayRefusedByCheckIsNotARelay(t *testing.T) {
	refuse := true
	side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		if _, err := relay(ctx, h, in.GetExecutionToken(), false, 1); status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "private address" {
			t.Errorf("refused relay: %v", err)
		}
		refuse = false
		if _, err := relay(ctx, h, in.GetExecutionToken(), false, 2); err != nil {
			t.Errorf("relay after a refusal: %v", err)
		}
		return hostUsage, nil
	}}
	_, a := executionInstance(t, side)
	res, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{}, core.ExecutionClient{Check: func(context.Context, *pluginv1.UpstreamRequest) error {
		if refuse {
			return core.ErrInvalidArgument.WithMessage("private address").WithCause(errors.New("10.0.0.1"))
		}
		return nil
	}})
	if err != nil || len(res.Relay.GetBody()) != 2 {
		t.Fatalf("relay %v err %v", res.Relay, err)
	}
}

// A relay that waits runs the exchange inside the host call and answers with
// its observation; it needs one of the two flags.
func TestRelayWaitReturnsObservation(t *testing.T) {
	var sends atomic.Int32
	client := core.ExecutionClient{Relay: func(_ context.Context, req *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
		sends.Add(1)
		return &pluginv1.RelayUpstreamResponse{Observation: &pluginv1.ExtractUsageRequest{Status: 200, Body: req.GetBody()}}, nil
	}}
	side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		out, err := relay(ctx, h, in.GetExecutionToken(), true, 7)
		if !in.GetExtractUsage() && !in.GetTaskSubmission() {
			if status.Code(err) != codes.FailedPrecondition {
				t.Errorf("wait without a flag: %v", err)
			}
			return hostUsage, nil
		}
		if err != nil || out.GetObservation().GetStatus() != 200 || out.GetError() != nil {
			t.Errorf("observation: %v %v", out, err)
		}
		return &pluginv1.ExecuteResponse{Report: &pluginv1.ExecuteResponse_Usage{Usage: &pluginv1.UsageReport{
			Tokens: &pluginv1.UsageTokens{InputTokens: int64(len(out.GetObservation().GetBody()))}}}}, nil
	}}
	_, a := executionInstance(t, side)
	for _, in := range []*pluginv1.ExecuteRequest{{ExtractUsage: true}, {TaskSubmission: true}} {
		res, err := a.Execute(context.Background(), in, client)
		if err != nil || res.Relay != nil || res.Response.GetUsage().GetTokens().GetInputTokens() != 7 {
			t.Fatalf("waited: %v %v", res, err)
		}
	}
	if sends.Load() != 2 {
		t.Fatalf("sends %d", sends.Load())
	}
	res, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{}, client)
	if err != nil || res.Relay != nil || sends.Load() != 2 {
		t.Fatalf("wait without a flag: %v %v sends %d", res, err, sends.Load())
	}
}

// The body of a relay may be as large as the larger of the request body
// limit and the body the plugin was handed, plus 1 MiB; the transport's
// headers are not the plugin's.
func TestRelayBodyBoundAndHeaders(t *testing.T) {
	side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		bound := max(mib, len(in.GetBody())) + mib
		if _, err := relay(ctx, h, in.GetExecutionToken(), false, bound+1); status.Code(err) != codes.InvalidArgument {
			t.Errorf("handed %d, relayed %d: %v", len(in.GetBody()), bound+1, err)
		}
		for _, name := range []string{"content-length", "Accept-Encoding", "connection", "transfer-encoding"} {
			for _, req := range []*pluginv1.UpstreamRequest{{Url: "https://u.example.com", Headers: map[string]string{name: "x"}}, {Url: "https://u.example.com", DefaultHeaders: map[string]string{name: "x"}}} {
				if _, err := h.RelayUpstream(ctx, &pluginv1.RelayUpstreamRequest{ExecutionToken: in.GetExecutionToken(), Request: req}); status.Code(err) != codes.InvalidArgument {
					t.Errorf("header %s: %v", name, err)
				}
			}
		}
		for _, req := range []*pluginv1.UpstreamRequest{nil, {}} {
			if _, err := h.RelayUpstream(ctx, &pluginv1.RelayUpstreamRequest{ExecutionToken: in.GetExecutionToken(), Request: req}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("request %v: %v", req, err)
			}
		}
		if _, err := relay(ctx, h, in.GetExecutionToken(), false, bound); err != nil {
			t.Errorf("handed %d, relayed %d: %v", len(in.GetBody()), bound, err)
		}
		return hostUsage, nil
	}}
	i, a := executionInstance(t, side)
	i.rt.o.RequestBodyLimit = func() int64 { return mib }
	for _, handed := range []int{0, mib, 3 * mib} {
		res, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{Body: make([]byte, handed)}, core.ExecutionClient{})
		if err != nil || res.Relay == nil {
			t.Fatalf("handed %d: %v %v", handed, res, err)
		}
	}
}

// Fetch: at most 8 per execution, only to the manifest's external services,
// a request body within the limit, an answer of at most 8 MiB, and no token
// outside its call or its process.
func TestFetchLimits(t *testing.T) {
	var fetches atomic.Int32
	var token string
	side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		token = in.GetExecutionToken()
		for _, url := range []string{"https://evil.example.org/x", "ftp://api.example.com/x", "https://api.example.com.evil.org/x", "://"} {
			if _, err := fetch(ctx, h, token, url, 0); status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v", url, err)
			}
		}
		if _, err := fetch(ctx, h, token, "https://api.example.com/x", mib+1); status.Code(err) != codes.InvalidArgument {
			t.Errorf("body above the limit: %v", err)
		}
		if _, err := fetch(ctx, (&hostServer{i: h.i, p: &proc{}}), token, "https://api.example.com/x", 0); status.Code(err) != codes.PermissionDenied {
			t.Errorf("another process: %v", err)
		}
		if fetches.Load() != 0 {
			t.Errorf("refused requests were sent: %d", fetches.Load())
		}
		out, err := fetch(ctx, h, token, "https://sub.API.example.com:8443/big", mib)
		if err != nil || !out.GetTruncated() || len(out.GetBody()) != core.ExecutionHTTPMaxResponseBytes {
			t.Errorf("oversized answer: truncated=%v bytes=%d err=%v", out.GetTruncated(), len(out.GetBody()), err)
		}
		for n := 2; n <= core.ExecutionHTTPMaxRequests; n++ {
			if out, err := fetch(ctx, h, token, "https://api.example.com/x", 0); err != nil || out.GetTruncated() || out.GetStatus() != 200 {
				t.Errorf("fetch %d: %v %v", n, out, err)
			}
		}
		if _, err := fetch(ctx, h, token, "https://api.example.com/x", 0); status.Code(err) != codes.ResourceExhausted {
			t.Errorf("fetch 9: %v", err)
		}
		return hostUsage, nil
	}}
	i, a := executionInstance(t, side)
	i.rt.o.RequestBodyLimit = func() int64 { return mib }
	client := core.ExecutionClient{Fetch: func(_ context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
		fetches.Add(1)
		if len(in.GetBody()) > 0 {
			return &pluginv1.ExecutionHTTPResponse{Status: 200, Body: make([]byte, core.ExecutionHTTPMaxResponseBytes+1)}, nil
		}
		return &pluginv1.ExecutionHTTPResponse{Status: 200, Body: []byte("ok")}, nil
	}}
	if _, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{}, client); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != core.ExecutionHTTPMaxRequests {
		t.Fatalf("fetches sent: %d", fetches.Load())
	}
	if _, err := fetch(context.Background(), side.h, token, "https://api.example.com/x", 0); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("token after Execute returned: %v", err)
	}
	if _, err := relay(context.Background(), side.h, token, false, 0); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("relay after Execute returned: %v", err)
	}
	if n := len(i.proc.Load().scopes.byToken); n != 0 {
		t.Fatalf("%d execution tokens leaked", n)
	}
	// An execution without a fetch capability refuses every fetch.
	side.execute = func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		if _, err := fetch(ctx, h, in.GetExecutionToken(), "https://api.example.com/x", 0); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("no capability: %v", err)
		}
		return hostUsage, nil
	}
	if _, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{}, core.ExecutionClient{}); err != nil {
		t.Fatal(err)
	}
}

// A fetch may also go to the hosts of the account's own upstream addresses -
// the address fields its type declares - and nowhere else. Whether such an
// address may be reached is still the guard's decision: one naming a private
// target passes the host rule and is refused by the SSRF policy.
func TestFetchAccountAddressHosts(t *testing.T) {
	account := &pluginv1.Account{Type: "apikey",
		SettingsJson:    `{"base_url":"https://Relay.Operator.example:8443/v1","note":"https://note.example.org"}`,
		CredentialsJson: `{"video_endpoint":"http://127.0.0.1:9/video","api_key":"https://key.example.org"}`}
	var sent atomic.Int32
	// The fetch of the usage module and the gateway: the URL goes through
	// the SSRF guard before anything is sent.
	public := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	guarded := func(ctx context.Context, in *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
		if _, err := netguard.CheckURL(ctx, in.GetUrl(), false, public); err != nil {
			return &pluginv1.ExecutionHTTPResponse{TransportError: "execution URL rejected by host policy"}, nil
		}
		sent.Add(1)
		return &pluginv1.ExecutionHTTPResponse{Status: 200}, nil
	}
	try := func(ctx context.Context, h *hostServer, token string) {
		for url, code := range map[string]codes.Code{
			"https://relay.operator.example/v1/tasks/1":     codes.OK, // an address of this account, any port
			"https://api.example.com/x":                     codes.OK, // externalServices, as before
			"https://sub.relay.operator.example/x":          codes.PermissionDenied,
			"https://note.example.org/x":                    codes.PermissionDenied, // not an address field
			"https://key.example.org/x":                     codes.PermissionDenied,
			"https://unrelated.example.net/x":               codes.PermissionDenied,
			"ftp://relay.operator.example/x":                codes.PermissionDenied,
			"https://relay.operator.example.evil.example/x": codes.PermissionDenied,
		} {
			if _, err := fetch(ctx, h, token, url, 0); status.Code(err) != code {
				t.Errorf("%s: %v, want %s", url, err, code)
			}
		}
		before := sent.Load()
		out, err := fetch(ctx, h, token, "http://127.0.0.1:9/video/tasks/1", 0)
		if err != nil || out.GetTransportError() == "" || out.GetStatus() != 0 || sent.Load() != before {
			t.Errorf("account address on a private target: %v %v, sent %d", out, err, sent.Load()-before)
		}
	}
	side := &pluginSide{
		execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
			try(ctx, h, in.GetExecutionToken())
			return hostUsage, nil
		},
		monitor: func(ctx context.Context, h *hostServer, in *pluginv1.PollRequest) (*pluginv1.MonitorResponse, error) {
			try(ctx, h, in.GetExecutionToken())
			return &pluginv1.MonitorResponse{Result: &pluginv1.ReconcileResult{}}, nil
		},
	}
	i, a := executionInstance(t, side)
	i.pkg.Manifest.AccountTypes[0].UpstreamFields = []string{"base_url", "video_endpoint"}
	if _, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{Account: account}, core.ExecutionClient{Fetch: guarded}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Monitor(context.Background(), &pluginv1.PollRequest{Account: account}, guarded); err != nil {
		t.Fatal(err)
	}
	if sent.Load() != 4 {
		t.Fatalf("sent %d", sent.Load())
	}
	// Another account, or a type that declares no address field, has only
	// the external services.
	i.pkg.Manifest.AccountTypes[0].UpstreamFields = nil
	side.execute = func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		if _, err := fetch(ctx, h, in.GetExecutionToken(), "https://relay.operator.example/v1", 0); status.Code(err) != codes.PermissionDenied {
			t.Errorf("no address fields: %v", err)
		}
		return hostUsage, nil
	}
	if _, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{Account: account}, core.ExecutionClient{Fetch: guarded}); err != nil {
		t.Fatal(err)
	}
}

// The platform call timeout bounds each stretch of the plugin's own work and
// never the time inside a host call.
func TestSegmentTimeout(t *testing.T) {
	const segment = 60 * time.Millisecond
	slowRelay := core.ExecutionClient{SegmentTimeout: segment, Relay: func(context.Context, *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
		time.Sleep(4 * segment) // the upstream exchange
		return &pluginv1.RelayUpstreamResponse{Observation: &pluginv1.ExtractUsageRequest{Status: 200}}, nil
	}}
	wait := func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	for _, tc := range []struct {
		name          string
		before, after time.Duration
		timeout       bool
	}{
		{"within", segment / 4, segment / 4, false},
		{"slow before the first host call", 4 * segment, 0, true},
		{"slow after the last host call", 0, 4 * segment, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
				if err := wait(ctx, tc.before); err != nil {
					return nil, err
				}
				if _, err := relay(ctx, h, in.GetExecutionToken(), true, 0); err != nil {
					return nil, err
				}
				if err := wait(ctx, tc.after); err != nil {
					return nil, err
				}
				return usage, nil
			}}
			_, a := executionInstance(t, side)
			res, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{ExtractUsage: true}, slowRelay)
			if !tc.timeout {
				if err != nil || res.Response.GetUsage() == nil {
					t.Fatalf("%v %v", res, err)
				}
				return
			}
			if core.AsError(err).Code != core.ErrPluginUnavailable.Code || reason(err) != core.PluginUnavailableSegmentTimeout || res.Response != nil {
				t.Fatalf("%v %v", res, err)
			}
		})
	}
}

// When the plugin gives up a waiting relay - its Execute returns or fails,
// or its host call is cancelled as for a crashed process - the exchange's
// context ends, and Execute returns only after the relay has ended.
func TestWaitingRelayIsCutAndJoined(t *testing.T) {
	for _, abandon := range []string{"execute returns", "host call cancelled"} {
		t.Run(abandon, func(t *testing.T) {
			started, cut, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			client := core.ExecutionClient{Relay: func(ctx context.Context, _ *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
				close(started)
				<-ctx.Done()
				close(cut)
				<-finish
				return nil, ctx.Err()
			}}
			side := &pluginSide{execute: func(ctx context.Context, h *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
				rpc, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); _, _ = relay(rpc, h, in.GetExecutionToken(), true, 0) }()
				<-started
				if _, err := fetch(ctx, h, in.GetExecutionToken(), "https://api.example.com/x", 0); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("host call during another: %v", err)
				}
				if abandon == "host call cancelled" {
					cancel()
					<-done
				}
				return nil, status.Error(codes.Unavailable, "transport is closing")
			}}
			_, a := executionInstance(t, side)
			returned := make(chan error, 1)
			go func() {
				_, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{ExtractUsage: true}, client)
				returned <- err
			}()
			<-cut
			select {
			case <-returned:
				t.Fatal("Execute returned while the relay still ran")
			case <-time.After(20 * time.Millisecond):
			}
			close(finish)
			if err := <-returned; core.AsError(err).Code != core.ErrPluginUnavailable.Code {
				t.Fatalf("execute: %v", err)
			}
		})
	}
}

// The bodies handed to an instance stay within a third of its memory limit;
// one that finds no room in time fails for a neutral reason; a body arriving
// alone is admitted whatever its size.
func TestInflightByteAdmission(t *testing.T) {
	const budget, body = mib, 400 << 10 // two bodies fit, three do not
	var inFlight, peak atomic.Int64
	release := make(chan struct{})
	side := &pluginSide{execute: func(_ context.Context, _ *hostServer, in *pluginv1.ExecuteRequest) (*pluginv1.ExecuteResponse, error) {
		n := inFlight.Add(int64(len(in.GetBody())))
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		<-release
		inFlight.Add(-int64(len(in.GetBody())))
		return hostUsage, nil
	}}
	i, a := executionInstance(t, side)
	i.proc.Load().limits.MemoryMB = 3
	if got := i.inflightBudget(); got != budget {
		t.Fatalf("budget %d", got)
	}
	// SegmentTimeout bounds the wait for room, not the blocked plugin: the
	// plugin side here ignores its context.
	run := func(size int, wait time.Duration) error {
		_, err := a.Execute(context.Background(), &pluginv1.ExecuteRequest{Body: make([]byte, size)}, core.ExecutionClient{SegmentTimeout: wait})
		return err
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() { errs <- run(body, 150*time.Millisecond) }()
	}
	var refused int
	for range 6 {
		if err := <-errs; core.AsError(err).Code == core.ErrPluginUnavailable.Code && reason(err) == core.PluginUnavailableInflightBytes {
			refused++
		}
	}
	if refused != 6 || peak.Load() != 2*body || i.bodies.inFlight() != 2*body {
		t.Fatalf("refused %d, peak %d, in flight %d", refused, peak.Load(), i.bodies.inFlight())
	}
	close(release)
	<-errs
	<-errs
	if i.bodies.inFlight() != 0 {
		t.Fatalf("in flight after all returned: %d", i.bodies.inFlight())
	}
	if err := run(5*budget, time.Second); err != nil {
		t.Fatalf("a lone body above the budget: %v", err)
	}
}

// Many bodies of mixed sizes: the sum in flight never passes the limit while
// more than one is in flight.
func TestByteBudgetNeverExceeded(t *testing.T) {
	const limit = 1000
	var b byteBudget
	var mu sync.Mutex
	var used, count int64
	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 200 {
				n := int64(1 + (g*37+k*101)%700)
				if g == 0 && k%50 == 0 {
					n = 3 * limit // admitted only alone
				}
				if !b.acquire(context.Background(), n, limit) {
					t.Error("acquire without a deadline failed")
					return
				}
				mu.Lock()
				used, count = used+n, count+1
				if used > limit && count > 1 {
					t.Errorf("%d bytes of %d in flight in %d bodies", used, limit, count)
				}
				mu.Unlock()
				mu.Lock()
				used, count = used-n, count-1
				mu.Unlock()
				b.release(n)
			}
		}()
	}
	wg.Wait()
	if b.inFlight() != 0 {
		t.Fatalf("left in flight: %d", b.inFlight())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if !b.acquire(ctx, 600, limit) || b.acquire(ctx, 600, limit) {
		t.Fatal("a second body over the limit was admitted")
	}
}

// Monitor's result is its return value. A terminal state needs a complete,
// successful, untruncated fetch made during the call.
func TestMonitorResult(t *testing.T) {
	settled, pending := pluginv1.ReconcileResult_SETTLED, pluginv1.ReconcileResult_PENDING
	for _, tc := range []struct {
		name   string
		answer *pluginv1.ExecutionHTTPResponse
		fetch  bool
		state  *pluginv1.ReconcileResult_State
		wantOK bool
	}{
		{"terminal with evidence", &pluginv1.ExecutionHTTPResponse{Status: 200}, true, &settled, true},
		{"terminal on an http error status", &pluginv1.ExecutionHTTPResponse{Status: 404}, true, &settled, true},
		{"terminal on a truncated answer", &pluginv1.ExecutionHTTPResponse{Status: 200, Truncated: true}, true, &settled, false},
		{"terminal on a transport error", &pluginv1.ExecutionHTTPResponse{TransportError: "offline"}, true, &settled, false},
		{"terminal without a fetch", nil, false, &settled, false},
		{"pending without a fetch", nil, false, &pending, true},
		{"no result", &pluginv1.ExecutionHTTPResponse{Status: 200}, true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			side := &pluginSide{monitor: func(ctx context.Context, h *hostServer, in *pluginv1.PollRequest) (*pluginv1.MonitorResponse, error) {
				if _, err := relay(ctx, h, in.GetExecutionToken(), false, 0); status.Code(err) != codes.PermissionDenied {
					t.Errorf("relay in Monitor: %v", err)
				}
				if tc.fetch {
					if _, err := fetch(ctx, h, in.GetExecutionToken(), "https://api.example.com/task", 0); err != nil {
						t.Errorf("fetch: %v", err)
					}
				}
				if tc.state == nil {
					return &pluginv1.MonitorResponse{}, nil
				}
				return &pluginv1.MonitorResponse{Result: &pluginv1.ReconcileResult{State: *tc.state}}, nil
			}}
			i, a := executionInstance(t, side)
			result, err := a.Monitor(context.Background(), &pluginv1.PollRequest{}, func(context.Context, *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
				return tc.answer, nil
			})
			if (err == nil) != tc.wantOK || (result != nil) != tc.wantOK || (tc.wantOK && result.GetState() != *tc.state) {
				t.Fatalf("result %v err %v", result, err)
			}
			if n := len(i.proc.Load().scopes.byToken); n != 0 {
				t.Fatalf("%d execution tokens leaked", n)
			}
		})
	}
}

// A fetch still running when Monitor returns is cancelled and joined, and
// cannot back the result that was produced before it ended.
func TestMonitorLateFetchIsNoEvidence(t *testing.T) {
	started := make(chan struct{})
	side := &pluginSide{monitor: func(_ context.Context, h *hostServer, in *pluginv1.PollRequest) (*pluginv1.MonitorResponse, error) {
		go func() {
			_, _ = fetch(context.Background(), h, in.GetExecutionToken(), "https://api.example.com/task", 0)
		}()
		<-started
		return &pluginv1.MonitorResponse{Result: &pluginv1.ReconcileResult{State: pluginv1.ReconcileResult_SETTLED}}, nil
	}}
	_, a := executionInstance(t, side)
	var ended atomic.Bool
	result, err := a.Monitor(context.Background(), &pluginv1.PollRequest{}, func(ctx context.Context, _ *pluginv1.ExecutionHTTPRequest) (*pluginv1.ExecutionHTTPResponse, error) {
		close(started)
		<-ctx.Done()
		ended.Store(true)
		return &pluginv1.ExecutionHTTPResponse{Status: 200}, nil
	})
	if err == nil || result != nil || !ended.Load() {
		t.Fatalf("result %v err %v, fetch ended %v", result, err, ended.Load())
	}
}

func TestMonitorNeedsCapabilityAndFetch(t *testing.T) {
	i, a := executionInstance(t, &pluginSide{})
	if _, err := a.Monitor(context.Background(), &pluginv1.PollRequest{}, nil); err == nil {
		t.Fatal("Monitor without a fetch capability")
	}
	delete(i.caps, manifest.CapPlatformMonitor)
	if _, err := a.Monitor(context.Background(), &pluginv1.PollRequest{}, ok200); err == nil {
		t.Fatal("Monitor without platform.monitor.v1")
	}
}

// The token is added to a copy that shares the caller's fields: the caller's
// message stays as it is and the body is not copied.
func TestShallowCloneSharesFields(t *testing.T) {
	in := &pluginv1.ExecuteRequest{Meta: &pluginv1.RequestMeta{RequestId: "r"}, Account: &pluginv1.Account{Id: 3}, Body: []byte("body"),
		InboundHeaders: map[string]string{"a": "b"}, Attempt: 2, ExtractUsage: true, TaskSubmission: true}
	out := shallowClone(in)
	out.ExecutionToken = "token"
	if in.GetExecutionToken() != "" || out.GetMeta() != in.GetMeta() || out.GetAccount() != in.GetAccount() || &out.GetBody()[0] != &in.GetBody()[0] ||
		out.GetInboundHeaders()["a"] != "b" || out.GetAttempt() != 2 || !out.GetExtractUsage() || !out.GetTaskSubmission() {
		t.Fatalf("clone %v of %v", out, in)
	}
}

// A platform plugin's memory limit is at least 64 MB plus three times the
// request body limit, under the global cap; a process started under another
// limit is stale.
func TestPlatformMemoryFloor(t *testing.T) {
	i, _ := executionInstance(t, &pluginSide{})
	spec := func(declared, cap int) int {
		i.pkg.Manifest.Resources = &manifest.Resources{MemoryMB: declared}
		i.rt.o.MaxMemoryMB = cap
		return i.launchSpec().MemoryMB
	}
	for _, tc := range []struct{ declared, cap, want int }{
		{128, 0, 448},      // default limit: 64 + 3*128
		{512, 0, 512},      // the manifest asks for more
		{128, 300, 300},    // the global cap wins
		{0, 0, 0},          // no limit stays no limit
		{128, 1024, 448},   //
		{2048, 1024, 1024}, //
	} {
		if got := spec(tc.declared, tc.cap); got != tc.want {
			t.Errorf("declared %d, cap %d: %d MB, want %d", tc.declared, tc.cap, got, tc.want)
		}
	}
	spec(128, 0)
	i.proc.Load().limits = limitsOf(i.launchSpec())
	if i.LimitsStale() {
		t.Fatal("stale under the limit it started with")
	}
	i.rt.o.RequestBodyLimit = func() int64 { return 256*mib + 1 }
	if got := i.launchSpec().MemoryMB; got != 64+3*257 || !i.LimitsStale() {
		t.Fatalf("after the limit changed: %d MB, stale %v", got, i.LimitsStale())
	}
	delete(i.caps, manifest.CapPlatformAdapter)
	if got := i.launchSpec().MemoryMB; got != 128 {
		t.Fatalf("a plugin that is no platform: %d MB", got)
	}
}

// Only the two calls that carry a request body may exceed 4 MiB.
func TestHostReceiveLimit(t *testing.T) {
	handled := func(context.Context, any) (any, error) { return "ok", nil }
	big := make([]byte, hostMaxRecvBytes+1)
	service := "/" + pluginv1.HostService_ServiceDesc.ServiceName + "/"
	for _, tc := range []struct {
		method string
		req    any
		code   codes.Code
	}{
		{"RelayUpstream", &pluginv1.RelayUpstreamRequest{Request: &pluginv1.UpstreamRequest{Body: big}}, codes.OK},
		{"ExecuteHTTP", &pluginv1.ExecutionHTTPRequest{Body: big}, codes.OK},
		{"Publish", &pluginv1.PublishRequest{Payload: big}, codes.ResourceExhausted},
		{"Publish", &pluginv1.PublishRequest{Payload: big[:mib]}, codes.OK},
	} {
		if _, err := limitHostRecv(context.Background(), tc.req, &grpc.UnaryServerInfo{FullMethod: service + tc.method}, handled); status.Code(err) != tc.code {
			t.Errorf("%s: %v", tc.method, err)
		}
	}
}

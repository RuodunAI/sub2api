package pluginsdktest

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// The fake applies the host's own checks, whatever the SDK checked before:
// a plugin that bypasses the SDK's helpers is held to the same contract.
func TestFakeHostRelayChecks(t *testing.T) {
	ctx := context.Background()
	ok := &pluginv1.UpstreamRequest{Url: "https://up.example.com/v1", Headers: map[string]string{"authorization": "k"}}
	f := NewFakeHost()
	relay := func(e *Execution, r *pluginv1.UpstreamRequest, wait bool) codes.Code {
		_, err := f.RelayUpstream(ctx, &pluginv1.RelayUpstreamRequest{ExecutionToken: e.Token(), Request: r, Wait: wait})
		return status.Code(err)
	}

	plain := f.Begin(&pluginv1.ExecuteRequest{})
	for name, tc := range map[string]struct {
		request *pluginv1.UpstreamRequest
		wait    bool
		want    codes.Code
	}{
		"upper-case header": {&pluginv1.UpstreamRequest{Url: ok.Url, Headers: map[string]string{"Authorization": "k"}}, false, codes.InvalidArgument},
		"transport header":  {&pluginv1.UpstreamRequest{Url: ok.Url, DefaultHeaders: map[string]string{"accept-encoding": "gzip"}}, false, codes.InvalidArgument},
		"relative url":      {&pluginv1.UpstreamRequest{Url: "/v1"}, false, codes.InvalidArgument},
		"wait without flag": {ok, true, codes.FailedPrecondition},
	} {
		if got := relay(plain, tc.request, tc.wait); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if len(plain.Relays()) != 0 || len(plain.RelayErrors()) != 4 {
		t.Fatalf("refused relays were recorded as relayed: %v %v", plain.Relays(), plain.RelayErrors())
	}
	if got := relay(plain, ok, false); got != codes.OK {
		t.Fatalf("first relay: %v", got)
	}
	if got := relay(plain, ok, false); got != codes.FailedPrecondition {
		t.Fatalf("second relay: %v", got)
	}
	if relays := plain.Relays(); len(relays) != 1 || relays[0].Wait || relays[0].Request != ok {
		t.Fatalf("relays: %v", relays)
	}

	waiting := f.Begin(&pluginv1.ExecuteRequest{TaskSubmission: true})
	if got := relay(waiting, ok, true); got != codes.Unimplemented {
		t.Fatalf("waited relay without RelayFunc: %v", got)
	}
	if got := relay(f.Begin(nil), ok, false); got != codes.FailedPrecondition {
		t.Fatalf("relay in Monitor: %v", got)
	}

	plain.End()
	if got := relay(plain, ok, false); got != codes.PermissionDenied {
		t.Fatalf("relay after the call returned: %v", got)
	}
	if _, err := f.ExecuteHTTP(ctx, &pluginv1.ExecutionHTTPRequest{ExecutionToken: "made-up", Url: ok.Url}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("fetch with a token the host never issued: %v", err)
	}
	if _, err := f.ExecuteHTTP(ctx, &pluginv1.ExecutionHTTPRequest{ExecutionToken: waiting.Token(), Url: "ftp://up.example.com"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("fetch of a non-http URL: %v", err)
	}
}

func TestRelayedFailsWithoutExactlyOneRelay(t *testing.T) {
	f := NewFakeHost()
	e := f.Begin(&pluginv1.ExecuteRequest{})
	probe := &fatalProbe{TB: t}
	e.Relayed(probe)
	if !probe.failed {
		t.Fatal("no relay passed Relayed")
	}
	_, _ = f.RelayUpstream(context.Background(), &pluginv1.RelayUpstreamRequest{ExecutionToken: e.Token(), Request: &pluginv1.UpstreamRequest{Url: "https://up.example.com"}})
	probe.failed = false
	if e.Relayed(probe); probe.failed {
		t.Fatal("one relay failed Relayed")
	}
	e.Err = status.Error(codes.Internal, "boom")
	if e.Relayed(probe); !probe.failed {
		t.Fatal("a failed call passed Relayed")
	}
}

type fatalProbe struct {
	testing.TB
	failed bool
}

func (p *fatalProbe) Helper()               {}
func (p *fatalProbe) Fatalf(string, ...any) { p.failed = true }

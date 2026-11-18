package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/execfetch"
)

// fetchDuringExecute runs fetch inside a plugin's Execute (before a normal
// waited relay) and returns what each request got.
func fetchDuringExecute(t *testing.T, reqs []*pluginv1.ExecutionHTTPRequest) []*pluginv1.ExecutionHTTPResponse {
	t.Helper()
	var got []*pluginv1.ExecutionHTTPResponse
	e, _ := scriptedUsageEnv(t, func(ctx context.Context, p *fakePlatform, in *pluginv1.ExecuteRequest, client core.ExecutionClient) (core.ExecutionResult, error) {
		for _, r := range reqs {
			out, err := client.Fetch(ctx, r)
			if err != nil {
				t.Errorf("fetch %s: %v", r.GetUrl(), err)
			}
			got = append(got, out)
		}
		return relayThen(func(*pluginv1.RelayUpstreamResponse) (*pluginv1.ExecuteResponse, error) {
			return &pluginv1.ExecuteResponse{Report: &pluginv1.ExecuteResponse_Usage{Usage: report(1, 1)}}, nil
		})(ctx, p, in, client)
	})
	if res := e.do("/pu/v1/chat", body(testModel, false), nil); res.status != 200 {
		t.Fatalf("status %d %s", res.status, res.body)
	}
	e.record()
	return got
}

// The gateway's fetches are held to the limits Monitor's are (execfetch):
// the host refuses Host, hop-by-hop and proxy headers, methods outside the
// list, oversized URLs and header sets, without sending anything; a failed
// exchange is a fixed text, and the answer's headers are capped.
func TestFetchLimits(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for i := range execfetch.MaxHeaders + 10 {
			w.Header().Set(fmt.Sprintf("X-H%d", i), "v")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + closed.Addr().String()
	_ = closed.Close()
	many := map[string]string{}
	for i := range execfetch.MaxHeaders + 1 {
		many[fmt.Sprintf("x-%d", i)] = "v"
	}
	refused := []*pluginv1.ExecutionHTTPRequest{
		{Url: srv.URL, Headers: map[string]string{"Host": "internal.example"}},
		{Url: srv.URL, Headers: map[string]string{"Upgrade": "websocket"}},
		{Url: srv.URL, Headers: map[string]string{"Proxy-Authorization": "secret"}},
		{Url: srv.URL, Headers: map[string]string{"TE": "trailers"}},
		{Url: srv.URL, Method: "CONNECT"},
		{Url: srv.URL + "/" + strings.Repeat("a", execfetch.MaxURLBytes)},
		{Url: srv.URL, Headers: many},
		{Url: srv.URL, Headers: map[string]string{"X-Big": strings.Repeat("v", execfetch.MaxHeaderBytes)}},
	}
	got := fetchDuringExecute(t, append(refused, &pluginv1.ExecutionHTTPRequest{Url: dead}, &pluginv1.ExecutionHTTPRequest{Url: srv.URL}))
	for i := range refused {
		if got[i].GetTransportError() == "" || got[i].GetStatus() != 0 {
			t.Errorf("refused request %d answered: %+v", i, got[i])
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("refused requests reached the server: %d hits", hits.Load())
	}
	if failed := got[len(refused)]; failed.GetTransportError() != execfetch.TransportFailure {
		t.Fatalf("transport failure text %q", failed.GetTransportError())
	}
	ok := got[len(refused)+1]
	if ok.GetStatus() != 200 || string(ok.GetBody()) != "ok" || !ok.GetTruncated() || len(ok.GetHeaders()) > execfetch.MaxHeaders {
		t.Fatalf("answer headers not capped: status %d truncated %v headers %d", ok.GetStatus(), ok.GetTruncated(), len(ok.GetHeaders()))
	}
}

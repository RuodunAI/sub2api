package gateway

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// recordingProxies hands out the direct client and records which proxy each
// request asked for (0: none); a proxy in down cannot be had.
type recordingProxies struct {
	fakeProxies
	mu    sync.Mutex
	asked []int64
	down  map[int64]bool
}

func (p *recordingProxies) HTTPClient(ctx context.Context, id *int64) (core.ProxyClient, error) {
	p.mu.Lock()
	p.asked = append(p.asked, proxyIdentity(id))
	down := id != nil && p.down[*id]
	p.mu.Unlock()
	if down {
		return core.ProxyClient{}, core.ErrUnavailable.WithMessage("proxy disabled")
	}
	return p.fakeProxies.HTTPClient(ctx, id)
}

// The relay goes out through the account's proxy as it stands when the
// request comes: a proxy changed between two requests carries the second,
// and one removed sends the third directly.
func TestRelayFollowsTheAccountsCurrentProxy(t *testing.T) {
	e := newEnv(t)
	proxies := &recordingProxies{}
	e.accounts.proxies = proxies
	setProxy := func(id *int64) {
		e.accounts.mu.Lock()
		e.accounts.accounts[1].ProxyID = id
		e.accounts.mu.Unlock()
	}
	first, second := int64(7), int64(8)
	for _, p := range []*int64{&first, &second, nil} {
		setProxy(p)
		if r := e.messages(body(testModel, false)); r.status != http.StatusOK {
			t.Fatalf("status %d %s", r.status, r.body)
		}
		e.record()
	}
	proxies.mu.Lock()
	defer proxies.mu.Unlock()
	if want := []int64{7, 8, 0}; !slices.Equal(proxies.asked, want) {
		t.Fatalf("proxies asked %v, want %v", proxies.asked, want)
	}
	// Each Execute was handed the account with the proxy its relay went
	// out through, password included (CONTRACTS §124).
	e.plat.mu.Lock()
	defer e.plat.mu.Unlock()
	if len(e.plat.builds) != 3 {
		t.Fatalf("executions = %d", len(e.plat.builds))
	}
	proxy := func(id int64) *pluginv1.AccountProxy { r := fakeProxy(id); return core.PluginProxy(&r) }
	for i, want := range []*pluginv1.AccountProxy{proxy(7), proxy(8), nil} {
		acct := e.plat.builds[i].GetAccount()
		if got := acct.GetProxy(); !proto.Equal(got, want) || acct.GetCredentialsJson() == "" {
			t.Errorf("execution %d: proxy %v, want %v", i, got, want)
		}
	}
}

// Without the account's egress (its proxy disabled) there is no Execute:
// the plugin is never handed the account without its proxy, and the
// request fails over.
func TestNoExecuteWithoutEgress(t *testing.T) {
	e := newEnv(t)
	down := int64(7)
	e.accounts.proxies = &recordingProxies{down: map[int64]bool{down: true}}
	e.accounts.mu.Lock()
	for _, acc := range e.accounts.accounts {
		acc.ProxyID = &down
	}
	e.accounts.mu.Unlock()
	if r := e.messages(body(testModel, false)); r.status == http.StatusOK {
		t.Fatalf("status %d %s", r.status, r.body)
	}
	if n := e.plat.buildCount(); n != 0 {
		t.Fatalf("executions without an egress = %d", n)
	}
}

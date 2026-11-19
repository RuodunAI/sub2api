package gateway

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"
)

// recordingProxies hands out the direct client and records which proxy each
// request asked for (0: none).
type recordingProxies struct {
	fakeProxies
	mu    sync.Mutex
	asked []int64
}

func (p *recordingProxies) HTTPClient(ctx context.Context, id *int64) (*http.Client, error) {
	p.mu.Lock()
	p.asked = append(p.asked, proxyIdentity(id))
	p.mu.Unlock()
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
}

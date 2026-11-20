package protocol

import (
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// The relay's rule, which the host's runtime, the gateway and the SDK all
// apply: lower-case names, none of the transport's (host included), method,
// names and values HTTP can carry, in both header maps.
func TestCheckRelayHeaders(t *testing.T) {
	ok := []*pluginv1.UpstreamRequest{
		{},
		{Method: "GET", Headers: map[string]string{"authorization": "Bearer k", "x-v": "a\tb"}, DefaultHeaders: map[string]string{"accept": "*/*"}},
	}
	for _, r := range ok {
		if err := CheckRelayHeaders(r); err != nil {
			t.Errorf("%v: %v", r, err)
		}
	}
	refused := []*pluginv1.UpstreamRequest{
		{Method: "BAD METHOD"},
		{Headers: map[string]string{"Authorization": "k"}},
		{DefaultHeaders: map[string]string{"": "v"}},
		{Headers: map[string]string{"bad name": "v"}},
		{DefaultHeaders: map[string]string{"x-v": "a\nb"}},
	}
	for name := range RelayTransportHeaders {
		refused = append(refused, &pluginv1.UpstreamRequest{Headers: map[string]string{name: "x"}}, &pluginv1.UpstreamRequest{DefaultHeaders: map[string]string{name: "x"}})
	}
	for _, r := range refused {
		if err := CheckRelayHeaders(r); err == nil {
			t.Errorf("%v accepted", r)
		}
	}
}

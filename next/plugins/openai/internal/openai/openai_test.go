package openai

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// TestGolden replays the frozen answers of the released plugin against the
// platform the plugin serves, with no exceptions.
func TestGolden(t *testing.T) {
	standardtest.Run(t, "openai", "../../manifest.json", New())
}

func TestCheck(t *testing.T) {
	if err := standard.Check(APIKey()); err != nil {
		t.Fatal(err)
	}
}

var routes = []string{openai.ProtocolChat, openai.ProtocolResponses, openai.ProtocolResponsesWS, openai.ProtocolEmbeddings}

// clientHeaders are what a client of the two OpenAI SDK headers sends.
var clientHeaders = map[string]string{"openai-organization": "org-client", "openai-project": "proj_client", "user-agent": "cli/1"}

// build is the upstream request of protocol for an account with settings.
func build(t *testing.T, p *standard.Platform, protocol, settings string, inbound map[string]string, body string) *pluginv1.UpstreamRequest {
	t.Helper()
	out, err := p.BuildUpstream(context.Background(), &pluginv1.ExecuteRequest{
		Meta:           &pluginv1.RequestMeta{Protocol: protocol, Model: "gpt-4o-mini", Stream: true},
		Account:        &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-test-0123456789"}`, SettingsJson: settings},
		InboundHeaders: inbound,
		Body:           []byte(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOrganizationAndProject covers the account's organization and project
// settings on every route, the WebSocket handshake included: a value set on
// the account replaces the client's header, and an account that sets
// neither (absent or left empty in the form) is answered exactly like the
// preset, the type the golden cases were recorded with, so the client's own headers still pass through and the
// golden cases hold for the plugin's type.
func TestOrganizationAndProject(t *testing.T) {
	plugin, baseline := New(), standard.New(openai.APIKey("apikey"))
	const body = `{"model":"gpt-4o-mini","stream":true}`
	for _, protocol := range routes {
		for _, inbound := range []map[string]string{nil, clientHeaders} {
			for _, settings := range []string{"", `{"organization":"","project":""}`} {
				got, want := build(t, plugin, protocol, settings, inbound, body), build(t, baseline, protocol, "", inbound, body)
				if !proto.Equal(got, want) {
					t.Errorf("%s, settings %q, client headers %v:\n got %v\nwant %v", protocol, settings, inbound, got, want)
				}
			}
			for settings, want := range map[string][2]string{
				`{"organization":"org-acct","project":"proj_acct"}`: {"org-acct", "proj_acct"},
				`{"organization":"org-acct"}`:                       {"org-acct", inbound["openai-project"]},
				`{"project":"proj_acct"}`:                           {inbound["openai-organization"], "proj_acct"},
			} {
				h := build(t, plugin, protocol, settings, inbound, body).GetDefaultHeaders()
				if got := [2]string{h["openai-organization"], h["openai-project"]}; got != want {
					t.Errorf("%s, settings %s, client headers %v: organization, project = %q, want %q", protocol, settings, inbound, got, want)
				}
			}
		}
	}
}

// TestForwardsEveryField pins the empty filter list of the released
// manifest: service_tier and safety_identifier reach the upstream unchanged.
func TestForwardsEveryField(t *testing.T) {
	const body = `{"model":"gpt-4o-mini", "service_tier":"priority","safety_identifier":"u1","input":[]}`
	for _, protocol := range []string{openai.ProtocolResponses, openai.ProtocolResponsesWS, openai.ProtocolEmbeddings} {
		if got := string(build(t, New(), protocol, "", nil, body).GetBody()); got != body {
			t.Errorf("%s body = %s, want %s", protocol, got, body)
		}
	}
}

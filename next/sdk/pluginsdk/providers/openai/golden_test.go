package openai_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// pluginType is the apikey type exactly as the openai plugin builds it:
// the preset without filter fields, plus the organization and project the
// account may set (they replace the client's headers of the same name).
func pluginType() standard.Type {
	t := openai.APIKey("apikey")
	t.FilterFields = nil
	t.Headers = append(t.Headers,
		standard.Set("openai-organization", standard.Field("organization")),
		standard.Set("openai-project", standard.Field("project")))
	return t
}

func TestGolden(t *testing.T) {
	goldentest.Run(t, "openai", standardtest.Golden(standard.New(pluginType())))
}

// The account's organization and project replace the client's headers of
// the same name; unset (or empty) they leave the request as the preset
// builds it, which is why the golden cases hold for the plugin's type
// (MANIFEST-ASSERTIONS.md, openai).
func TestOrganizationAndProject(t *testing.T) {
	withFields, preset := standard.New(pluginType()), standard.New(noFilter(openai.APIKey("apikey")))
	build := func(p *standard.Platform, protocol, settings string, inbound map[string]string) *pluginv1.UpstreamRequest {
		t.Helper()
		out, err := p.BuildUpstream(context.Background(), &pluginv1.ExecuteRequest{
			Meta:           &pluginv1.RequestMeta{Protocol: protocol, Model: "gpt-4o-mini", Stream: true},
			Account:        &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-test-0123456789"}`, SettingsJson: settings},
			InboundHeaders: inbound, Body: []byte(`{"model":"gpt-4o-mini","stream":true}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	client := map[string]string{"openai-organization": "org-client", "openai-project": "proj_client", "user-agent": "cli/1"}
	for _, protocol := range []string{openai.ProtocolChat, openai.ProtocolResponses, openai.ProtocolResponsesWS, openai.ProtocolEmbeddings} {
		for _, inbound := range []map[string]string{nil, client} {
			for _, settings := range []string{"", `{"organization":"","project":""}`} {
				if got, want := build(withFields, protocol, settings, inbound), build(preset, protocol, settings, inbound); !proto.Equal(got, want) {
					t.Errorf("%s %v %s: %v, want %v", protocol, inbound, settings, got, want)
				}
			}
			for settings, want := range map[string][2]string{
				`{"organization":"org-acct","project":"proj_acct"}`: {"org-acct", "proj_acct"},
				`{"organization":"org-acct"}`:                       {"org-acct", inbound["openai-project"]},
				`{"project":"proj_acct"}`:                           {inbound["openai-organization"], "proj_acct"},
			} {
				h := build(withFields, protocol, settings, inbound).DefaultHeaders
				if h["openai-organization"] != want[0] || h["openai-project"] != want[1] {
					t.Errorf("%s %v %s: %v", protocol, inbound, settings, h)
				}
			}
		}
	}
}

func noFilter(t standard.Type) standard.Type { t.FilterFields = nil; return t }

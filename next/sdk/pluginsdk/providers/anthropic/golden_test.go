package anthropic_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// anthropicType is the apikey type of the anthropic plugin: the preset
// without filter fields.
func anthropicType() standard.Type {
	t := anthropic.APIKey("apikey")
	t.FilterFields = nil
	return t
}

// relayType is the relay plugin's relay_key: the anthropic preset with a
// required base URL and no default, its own wording for it, no default
// protocol, no filter fields and only two client headers.
func relayType() standard.Type {
	t := anthropic.APIKey("relay_key")
	t.Spec.DefaultBaseURL, t.Spec.RequireBaseURL = "", true
	t.Spec.Texts.BaseURLRequired = "Base URL of the relay is required / 请填写中转站的 Base URL"
	t.DefaultProtocol = ""
	t.FilterFields = nil
	t.PassHeaders = []string{"anthropic-version", "anthropic-beta"}
	return t
}

func TestGolden(t *testing.T) {
	goldentest.Run(t, "anthropic", standardtest.Golden(standard.New(anthropicType())))
}

func TestGoldenRelay(t *testing.T) {
	goldentest.Run(t, "relay", standardtest.Golden(standard.New(relayType())))
}

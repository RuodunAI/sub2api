package main

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest/conformance"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
)

// keptAboveOperator is the reason of every authoritative header that is not
// the credential: the plugin was released with all of its headers written
// above the operator's passthrough and overrides, and keeps that.
const keptAboveOperator = "released code plugin: written above operator settings before forwarding v2; behaviour kept"

// TestConformance runs the platform conformance suite on the request-path
// inputs of executeCases: both account types, messages streaming and not,
// and count_tokens.
func TestConformance(t *testing.T) {
	var cases []conformance.Case
	for _, tc := range executeCases() {
		cases = append(cases, conformance.Case{Name: tc.name, Account: tc.in.GetAccount(), Meta: tc.in.GetMeta(), Body: tc.in.GetBody()})
	}
	conformance.Run(t, conformance.Plugin{
		Platform:    newPlugin(),
		Manifest:    manifestRaw,
		Protocols:   []string{anthropic.ProtocolMessages, anthropic.ProtocolCountTokens},
		Credentials: []string{"authorization"},
		Authoritative: map[string]string{
			"anthropic-version": keptAboveOperator,
			"anthropic-beta":    keptAboveOperator + "; the oauth beta is what an OAuth token is accepted with",
			"content-type":      keptAboveOperator,
		},
	}, cases...)
}

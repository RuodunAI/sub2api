package main

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/plugins/relay/internal/relay"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest/conformance"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Plugin{
		Platform:    relay.New(),
		SDK:         []pluginsdk.Option{pluginsdk.WithManifest(manifestJSON)},
		Protocols:   []string{anthropic.ProtocolMessages, anthropic.ProtocolCountTokens},
		PassHeaders: relay.RelayKey().PassHeaders,
		Credentials: []string{"x-api-key"},
	}, conformanceCases()...)
}

// conformanceCases are a stream and a non-stream request of each protocol.
func conformanceCases() []conformance.Case {
	account := &pluginv1.Account{Id: 1, Type: relay.TypeRelayKey,
		CredentialsJson: `{"api_key":"sk-relay-conformance"}`, SettingsJson: `{"base_url":"https://relay.example.com"}`}
	var cases []conformance.Case
	for _, protocol := range []string{anthropic.ProtocolMessages, anthropic.ProtocolCountTokens} {
		for _, stream := range []bool{false, true} {
			body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			cases = append(cases, conformance.Case{Account: account,
				Meta: &pluginv1.RequestMeta{Protocol: protocol, Model: "claude-haiku-4-5", Stream: stream}, Body: []byte(body)})
		}
	}
	return cases
}

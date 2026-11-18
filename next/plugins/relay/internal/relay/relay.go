// Package relay is the platform of the relay_key account type: an
// Anthropic-compatible relay at the account's own base URL, for the built-in
// anthropic platform.
package relay

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// TypeRelayKey is the account type id (manifest.json accountTypes[0].id).
const TypeRelayKey = "relay_key"

// BaseURLRequired is the message of an account without a base URL.
const BaseURLRequired = "Base URL of the relay is required / 请填写中转站的 Base URL"

// New returns the platform of the relay_key account type.
func New() *standard.Platform { return standard.New(RelayKey()) }

// RelayKey is the anthropic preset on the relay's own address: the base URL
// is required and has no default, a request must name its protocol, the
// plugin forwards every field, and only the two client headers the manifest
// passes reach the relay.
func RelayKey() standard.Type {
	t := anthropic.APIKey(TypeRelayKey)
	t.Spec.DefaultBaseURL, t.Spec.RequireBaseURL = "", true
	t.Spec.Texts.BaseURLRequired = BaseURLRequired
	t.DefaultProtocol = ""
	t.PassHeaders = []string{"anthropic-version", "anthropic-beta"}
	return t
}

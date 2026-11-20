// Package anthropic is the Anthropic API key platform: the anthropic preset
// of sdk/pluginsdk/standard as it is. The anthropic platform and its
// endpoints are built into the core (sdk/platforms); this plugin declares
// none.
package anthropic

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(APIKey()) }

// APIKey is the apikey type: the anthropic preset (every request field is
// forwarded), its credentials api_key and base_url only.
func APIKey() standard.Type { return anthropic.APIKey("apikey") }

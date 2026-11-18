// Package openai is the OpenAI API key platform: the openai preset with the
// account's organization and project, forwarding every request field.
package openai

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(APIKey()) }

// APIKey is the apikey type: the preset, plus the organization and project
// an account may set, which replace the client's headers.
func APIKey() standard.Type {
	t := openai.APIKey("apikey")
	t.Headers = append(t.Headers,
		standard.Set("openai-organization", standard.Field("organization")),
		standard.Set("openai-project", standard.Field("project")))
	return t
}

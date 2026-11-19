package azureopenai

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey/apikeytest"
)

// TestCredentialsRefused: the account forms' rules (base_url and api_key
// required; api_key 8-512 characters of [!-~]; base_url an http(s) URL
// without whitespace, query or fragment that matches EndpointPattern once
// normalized; azure_deployment also api_version, required, matching
// APIVersionPattern; no other key) hold in ValidateCredentials, which is
// final: the core no longer checks a schema. The shared api_key + base_url
// cases of both types, then each type's own.
func TestCredentialsRefused(t *testing.T) {
	const key, endpoint = "0123456789abcdef", "https://res.openai.azure.com"
	v1 := apikeytest.Form{AccountType: TypeV1, Key: key, BaseURL: endpoint, RequireBaseURL: true, MinKey: 8}
	deployment := apikeytest.Form{AccountType: TypeDeployment, Key: key, BaseURL: endpoint, RequireBaseURL: true, MinKey: 8,
		Fill: map[string]any{"api_version": "2024-10-21"}}
	cases := append(v1.Cases(), deployment.Cases()...)
	cases = append(cases, endpointCases(TypeV1, ``, `{"api_key":"`+key+`","base_url":"`+endpoint+`"}`)...)
	cases = append(cases, endpointCases(TypeDeployment, `"api_version": "2024-10-21", `, `{"api_key":"`+key+`","api_version":"2024-10-21","base_url":"`+endpoint+`"}`)...)
	deploymentCase := func(name, apiVersion, field, code string) apikeytest.Case {
		return apikeytest.Case{Name: name, AccountType: TypeDeployment, Creds: `{"api_key": "` + key + `", "base_url": "` + endpoint + `"` + apiVersion + `}`, Field: field, Code: code}
	}
	apikeytest.Run(t, New().ValidateCredentials, append(cases,
		apikeytest.Case{Name: "api_version, a key of azure_deployment only", AccountType: TypeV1, Creds: `{"api_key": "` + key + `", "base_url": "` + endpoint + `", "api_version": "2024-10-21"}`, Field: "api_version", Code: "unknown"},
		deploymentCase("a key the form never sends", `, "api_version": "2024-10-21", "legacy": "x"`, "legacy", "unknown"),
		deploymentCase("api_version missing", ``, "api_version", "required"),
		deploymentCase("api_version empty", `, "api_version": ""`, "api_version", "required"),
		deploymentCase("api_version not a string", `, "api_version": 20241021`, "api_version", ""),
		deploymentCase("api_version not a date", `, "api_version": "latest"`, "api_version", "pattern"),
		deploymentCase("api_version with another suffix", `, "api_version": "2024-10-21-beta"`, "api_version", ""),
		deploymentCase("a preview api_version", `, "api_version": "2025-04-01-preview"`, "", ""),
	))
}

// endpointCases are the base_url cases EndpointPattern adds on the type
// typ, fill the JSON of its other fields: a pasted request URL refused, the
// endpoint forms accepted, and the pattern matched on the URL as
// normalized, so surrounding spaces and an upper-case scheme are trimmed
// and lower-cased before it, and the suffix removed: saved is what is saved.
func endpointCases(typ, fill, saved string) []apikeytest.Case {
	c := func(name, base, field, want string) apikeytest.Case {
		return apikeytest.Case{Name: name, AccountType: typ, Creds: `{` + fill + `"api_key": "0123456789abcdef", "base_url": "` + base + `"}`, Field: field, Want: want}
	}
	return []apikeytest.Case{
		c("base_url with a deployment", "https://res.openai.azure.com/openai/deployments/gpt-4o", "base_url", ""),
		c("base_url with an operation", "https://res.openai.azure.com/openai/v1/chat/completions", "base_url", ""),
		c("base_url with another path", "https://res.openai.azure.com/other", "base_url", ""),
		c("a Foundry project endpoint", "https://res.services.ai.azure.com/api/projects/p1", "", ""),
		c("base_url normalized, then matched", " HTTPS://res.openai.azure.com/openai/v1/ ", "", saved),
	}
}

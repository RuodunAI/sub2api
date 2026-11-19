package typesafe

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey/apikeytest"
)

// TestCredentialsRefused: the account form's rules (api_key a non-empty
// string; an optional base_url, an http(s) URL without whitespace, query or
// fragment, saved as written; no other key) hold in ValidateCredentials,
// which is final: the core no longer checks a schema. The shared api_key +
// base_url cases, and a key the form never sends.
func TestCredentialsRefused(t *testing.T) {
	form := apikeytest.Form{AccountType: "apikey", Key: "ts-abcdefgh", BaseURL: "https://x.example.com"}
	apikeytest.Run(t, New().ValidateCredentials, append(form.Cases(),
		apikeytest.Case{Name: "a key the form never sends", AccountType: "apikey", Creds: `{"api_key": "ts-abcdefgh", "base_url": "https://x.example.com", "legacy": "x"}`, Field: "legacy", Code: "unknown"},
	))
}

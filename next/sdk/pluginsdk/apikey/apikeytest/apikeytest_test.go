package apikeytest_test

import (
	"context"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey/apikeytest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/fieldcheck"
)

func validate(s apikey.Spec) func(context.Context, *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	return func(_ context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
		return s.Validate(in), nil
	}
}

// The shared cases hold for the SDK's own api_key + base_url check, with
// base_url optional or required, with fields of the type's own.
func TestCases(t *testing.T) {
	optional := apikeytest.Form{AccountType: "apikey", Key: "sk-abcdefgh", BaseURL: "https://x.example.com/a", MinKey: 8}
	apikeytest.Run(t, validate(apikey.Spec{AccountType: "apikey", DefaultBaseURL: "https://api.example.com"}), optional.Cases())

	required := apikeytest.Form{AccountType: "relay", Key: "sk-abcdefgh", BaseURL: "http://relay.example.com", RequireBaseURL: true, MinKey: 8, Fill: map[string]any{"api_version": "2024-10-21"}}
	spec := apikey.Spec{AccountType: "relay", RequireBaseURL: true, Fields: fieldcheck.Fields{"api_version": fieldcheck.String().Required()}}
	apikeytest.Run(t, validate(spec), append(required.Cases(),
		apikeytest.Case{Name: "api_version missing", AccountType: "relay", Creds: `{"api_key":"sk-abcdefgh","base_url":"http://relay.example.com"}`, Field: "api_version", Code: "required"},
		apikeytest.Case{Name: "unknown key", AccountType: "relay", Creds: `{"api_key":"sk-abcdefgh","base_url":"http://relay.example.com","api_version":"v","x":1}`, Field: "x", Code: "unknown"},
	))
}

// A form with a longer shortest key: the openai preset (sk- or sess-, at
// least 10), bounded to 512 visible characters by a rule on api_key (it sees
// the key trimmed, as saved).
func TestMinKey(t *testing.T) {
	v := apikey.OpenAI
	spec := apikey.Spec{AccountType: "apikey", KeyValidator: &v, Fields: fieldcheck.Fields{
		"api_key":      fieldcheck.String().MaxLen(512).Pattern(`^[!-~]*$`),
		"organization": fieldcheck.String().MaxLen(128).Pattern(`^[!-~]*$`),
	}}
	form := apikeytest.Form{AccountType: "apikey", Key: "sk-abcdefgh", BaseURL: "https://x.example.com", MinKey: 10}
	apikeytest.Run(t, validate(spec), append(form.Cases(),
		apikeytest.Case{Name: "organization with a space", AccountType: "apikey", Creds: `{"api_key":"sk-abcdefgh","organization":"o o"}`, Field: "organization", Code: "pattern"},
	))
}

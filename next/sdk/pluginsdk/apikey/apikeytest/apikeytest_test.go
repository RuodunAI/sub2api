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
	optional := apikeytest.Form{AccountType: "apikey", Key: "sk-abcdefgh", BaseURL: "https://x.example.com/a"}
	apikeytest.Run(t, validate(apikey.Spec{AccountType: "apikey", DefaultBaseURL: "https://api.example.com", Versions: []string{"/v1"}}), optional.Cases())

	required := apikeytest.Form{AccountType: "relay", Key: "sk-abcdefgh", BaseURL: "http://relay.example.com", RequireBaseURL: true, Fill: map[string]any{"api_version": "2024-10-21"}}
	spec := apikey.Spec{AccountType: "relay", RequireBaseURL: true, Fields: fieldcheck.Fields{"api_version": fieldcheck.String().Required()}}
	apikeytest.Run(t, validate(spec), append(required.Cases(),
		apikeytest.Case{Name: "api_version missing", AccountType: "relay", Creds: `{"api_key":"sk-abcdefgh","base_url":"http://relay.example.com"}`, Field: "api_version", Code: "required"},
		apikeytest.Case{Name: "unknown key", AccountType: "relay", Creds: `{"api_key":"sk-abcdefgh","base_url":"http://relay.example.com","api_version":"v","x":1}`, Field: "x", Code: "unknown"},
	))
}

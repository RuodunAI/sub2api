// Package openai is the OpenAI API key platform: the openai preset with the
// account's organization and project, forwarding every request field.
package openai

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/apikey"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/fieldcheck"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// TypeAPIKey is the account type id (accountTypes[].id of manifest.json).
const TypeAPIKey = "apikey"

// Platform is the standard platform of the apikey type, its credentials
// also checked for what the preset's key rule leaves out (see
// ValidateCredentials).
type Platform struct{ *standard.Platform }

// New returns the platform of the apikey account type.
func New() *Platform { return &Platform{standard.New(APIKey())} }

// APIKey is the apikey type: the preset, plus the organization and project
// an account may set, which replace the client's headers. No other key is
// accepted; Platform.ValidateCredentials checks the two.
func APIKey() standard.Type {
	t := openai.APIKey(TypeAPIKey)
	t.Spec.Fields = fieldcheck.Fields{"organization": fieldcheck.Any(), "project": fieldcheck.Any()}
	t.Headers = append(t.Headers,
		standard.Set("openai-organization", standard.Field("organization")),
		standard.Set("openai-project", standard.Field("project")))
	return t
}

// ValidateCredentials is the preset's check (an sk- or sess- key of at
// least 10 characters, the base URL) plus the rest of the account form's
// rules: a key of at most 512 visible ASCII characters, and an organization
// and a project of at most 128 (empty allowed), sent as they are in headers.
func (p *Platform) ValidateCredentials(ctx context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	resp, err := p.Platform.ValidateCredentials(ctx, in)
	if err != nil || in.GetAccountType() != TypeAPIKey {
		return resp, err
	}
	creds, errC := apikey.DecodeObject(in.GetCredentialsJson())
	settings, errS := apikey.DecodeObject(in.GetSettingsJson())
	if errC != nil || errS != nil {
		return resp, nil // the preset refused it
	}
	var errs pluginsdk.FieldErrors
	keyRefused := slices.ContainsFunc(resp.GetErrors(), func(e *pluginv1.FieldError) bool { return e.GetField() == "api_key" })
	v, _ := apikey.Lookup("api_key", creds, settings)
	if key, _ := v.(string); !keyRefused && !visible(strings.TrimSpace(key), 512) {
		errs = errs.Add("api_key", "pattern", "API key must be 8-512 printable characters without spaces / API Key 须为 8-512 个不含空格的可见字符")
	}
	for _, k := range []string{"organization", "project"} {
		v, present := apikey.Lookup(k, settings, creds)
		if s, ok := v.(string); present && (!ok || !visible(s, 128)) {
			errs = errs.Add(k, "pattern", k+" must be at most 128 printable characters without spaces / "+k+" 须为不超过 128 个不含空格的可见字符")
		}
	}
	if len(errs) == 0 {
		return resp, nil
	}
	return &pluginv1.ValidateCredentialsResponse{Errors: append(resp.GetErrors(), errs...)}, nil
}

// visible reports whether s is at most max characters, each a visible
// ASCII one ([!-~]).
func visible(s string, max int) bool {
	if utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if r < '!' || r > '~' {
			return false
		}
	}
	return true
}

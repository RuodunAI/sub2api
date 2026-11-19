// Package apikeytest holds the ValidateCredentials cases every api_key +
// base_url account type shares, so a plugin's test lists only its own:
//
//	func TestCredentialsRefused(t *testing.T) {
//		form := apikeytest.Form{AccountType: "apikey", Key: "sk-ant-api03-abcdef", BaseURL: "https://x.example.com", MinKey: 8}
//		apikeytest.Run(t, New().ValidateCredentials, append(form.Cases(),
//			apikeytest.Case{Name: "organization not a string", AccountType: "apikey", Creds: `{"api_key": "sk-proj-abcdef", "organization": 1}`, Field: "organization"},
//		))
//	}
package apikeytest

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// Case is one ValidateCredentials request (credentials only) and its
// verdict: refused with its first error at Field (and Code, when set), or,
// with Field empty, accepted, the normalized credentials equal to Want when
// it is set.
type Case struct {
	Name        string
	AccountType string
	Creds       string
	Field, Code string
	Want        string
}

// Form is an account type with api_key and base_url: a key and a base URL
// it accepts (the URL as the type normalizes it), whether base_url is
// required, the shortest key it accepts (the longest is 512 characters, the
// SDK's rule) and the other fields of a valid account.
type Form struct {
	AccountType    string
	Key            string
	BaseURL        string
	RequireBaseURL bool
	MinKey         int
	Fill           map[string]any
}

// Cases are the shared cases: each way api_key and base_url are refused,
// the key lengths at the bounds accepted, and the key and the URL trimmed
// and the scheme lower-cased on save. The other fields are Fill's; the
// key cases carry the base URL.
func (f Form) Cases() []Case {
	keyOf := func(n int) string { return "sk-" + strings.Repeat("k", n-3) }
	withKey := func(key any) map[string]any { return f.with(map[string]any{"api_key": key, "base_url": f.BaseURL}) }
	withURL := func(url any) map[string]any { return f.with(map[string]any{"api_key": f.Key, "base_url": url}) }
	refused := func(name, field string, creds map[string]any) Case {
		return Case{Name: name, AccountType: f.AccountType, Creds: encode(creds), Field: field}
	}
	accepted := func(name string, creds, want map[string]any) Case {
		c := Case{Name: name, AccountType: f.AccountType, Creds: encode(creds)}
		if want != nil {
			c.Want = encode(want)
		}
		return c
	}
	missingKey := withKey(nil)
	delete(missingKey, "api_key")
	cases := []Case{
		refused("api_key missing", "api_key", missingKey),
		refused("api_key empty", "api_key", withKey("")),
		refused("api_key not a string", "api_key", withKey(123456789)),
		refused("api_key shorter than the shortest", "api_key", withKey(keyOf(f.MinKey-1))),
		refused("api_key longer than 512", "api_key", withKey(keyOf(513))),
		refused("api_key with a space", "api_key", withKey("sk-abc def")),
		refused("api_key not ASCII", "api_key", withKey("sk-ключ-abcdef")),
		refused("base_url not a string", "base_url", withURL(1)),
		refused("base_url not http", "base_url", withURL("ftp://x.example.com")),
		refused("base_url without a scheme", "base_url", withURL("x.example.com")),
		refused("base_url without a host", "base_url", withURL("https://")),
		refused("base_url with a query", "base_url", withURL("https://x.example.com/?a=1")),
		refused("base_url with a fragment", "base_url", withURL("https://x.example.com/#a")),
		refused("base_url with a space", "base_url", withURL("https://x.example.com/a b")),
		accepted("api_key of the shortest length", withKey(keyOf(f.MinKey)), nil),
		accepted("api_key of 512 characters", withKey(keyOf(512)), nil),
		accepted("api_key trimmed", withKey(" "+f.Key+" "), withURL(f.BaseURL)),
		accepted("base_url trimmed", withURL(" "+f.BaseURL+" "), withURL(f.BaseURL)),
		accepted("base_url scheme lower-cased", withURL(strings.ToUpper(f.BaseURL[:5])+f.BaseURL[5:]), withURL(f.BaseURL)),
	}
	if f.RequireBaseURL {
		missingURL := withURL(nil)
		delete(missingURL, "base_url")
		cases = append(cases, refused("base_url missing", "base_url", missingURL), refused("base_url empty", "base_url", withURL("")))
	}
	return cases
}

// with is Fill plus the given fields.
func (f Form) with(fields map[string]any) map[string]any {
	out := maps.Clone(f.Fill)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, fields)
	return out
}

func encode(v map[string]any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Run checks each case against validate (a platform's ValidateCredentials).
func Run(t *testing.T, validate func(context.Context, *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error), cases []Case) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.AccountType+"/"+c.Name, func(t *testing.T) {
			resp, err := validate(context.Background(), &pluginv1.ValidateCredentialsRequest{AccountType: c.AccountType, CredentialsJson: c.Creds})
			if err != nil {
				t.Fatalf("%s: %v", c.Creds, err)
			}
			errs := resp.GetErrors()
			switch {
			case c.Field != "":
				if len(errs) == 0 || errs[0].GetField() != c.Field || (c.Code != "" && errs[0].GetCode() != c.Code) {
					t.Errorf("%s: errors = %v, want one at %s %s", c.Creds, errs, c.Field, c.Code)
				}
			case len(errs) != 0:
				t.Errorf("%s: errors = %v, want none", c.Creds, errs)
			case c.Want != "" && resp.GetNormalizedCredentialsJson() != c.Want:
				t.Errorf("%s: saved %s, want %s", c.Creds, resp.GetNormalizedCredentialsJson(), c.Want)
			}
		})
	}
}

package azureopenai

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
	"github.com/Sub2API-Devs/sup2api/next/sdk/upstream"
)

// The two account types (DESIGN-2026-11-17-CLOUD-PROVIDERS §6): the same
// resource endpoint and key, served through the v1 API or the classic
// per-deployment API. They are two types because a route cannot branch on
// an account setting.
const (
	typeV1         = "azure_v1"
	typeDeployment = "azure_deployment"
)

// shared is what both declarations put over the openai preset: a required
// resource endpoint, any key, the api-key header, no model listing.
const shared = `
	"preset": "openai",
	"baseURL": {"default": null, "required": true, "stripSuffixes": ["/openai/v1", "/openai"]},
	"auth": {"type": "header", "name": "api-key", "value": "{api_key}"},
	"filterFields": [],
	"models": null,`

// declarations are the upstreams of the two types.
var declarations = map[string]string{
	typeV1: `{` + shared + `
		"credentials": {"keyValidator": null, "texts": {"baseURLRequired": "Resource endpoint is required / 请填写资源地址"}},
		"routes": {
			"openai.chat": {"path": "/openai/v1/chat/completions"},
			"openai.responses": {"path": "/openai/v1/responses"},
			"openai.embeddings": {"path": "/openai/v1/embeddings"},
			"openai.responses_ws": null
		}
	}`,
	typeDeployment: `{` + shared + `
		"credentials": {"keyValidator": null, "texts": {"baseURLRequired": "Resource endpoint is required / 请填写资源地址"}, "fields": ["api_version"]},
		"routes": {
			"openai.chat": {"path": "/openai/deployments/{model}/chat/completions?api-version={credentials.api_version}", "model": {"required": true}},
			"openai.embeddings": {"path": "/openai/deployments/{model}/embeddings?api-version={credentials.api_version}", "model": {"required": true}},
			"openai.responses": null,
			"openai.responses_ws": null
		}
	}`,
}

// endpoints are the openai endpoints each type serves; the routes above
// must match them (sdk/manifest/check).
var endpoints = map[string][]string{
	typeV1:         {"chat_completions", "responses", "embeddings"},
	typeDeployment: {"chat_completions", "embeddings"},
}

const (
	resource = "https://res.openai.azure.com"
	apiKey   = "0123456789abcdef0123456789abcdef" // an Azure key has no prefix
)

// TestManifest runs manifest.json through the checks the core runs before
// it installs a package (sdk/manifest/check in tooling mode, which also
// resolves the upstream declarations with the core's engine) and pins the
// package: account types only, no process, one creation group whose first
// variant is the v1 API, an unguarded resource endpoint (as for relay,
// CONTRACTS §21.3.1) and no prefilled models (a model is a deployment name).
func TestManifest(t *testing.T) {
	m := load(t)
	if m.Key != "azure_openai" || m.Version != "0.1.0" || m.Publisher != "sub2api" || m.Runtime != manifest.RuntimeNone {
		t.Fatalf("identity = %s %s %s %s", m.Key, m.Version, m.Publisher, m.Runtime)
	}
	if len(m.Platforms) != 0 || len(m.AccountTypes) != 2 || m.AccountTypes[0].ID != typeV1 || m.AccountTypes[1].ID != typeDeployment {
		t.Fatalf("platforms = %+v accountTypes = %+v, want %s then %s only", m.Platforms, m.AccountTypes, typeV1, typeDeployment)
	}
	for _, at := range m.AccountTypes {
		if at.CreationGroup != "azure-openai" || at.AuthMethodLabel["en"] == "" || at.AuthMethodLabel["zh"] == "" {
			t.Errorf("%s: creationGroup %q authMethodLabel %v", at.ID, at.CreationGroup, at.AuthMethodLabel)
		}
		if len(at.Platforms) != 1 || at.Platforms[0].Platform != "openai" || !slices.Equal(at.Platforms[0].Endpoints, endpoints[at.ID]) {
			t.Errorf("%s: platforms = %+v, want openai %v", at.ID, at.Platforms, endpoints[at.ID])
		}
		if !slices.Equal(at.SensitiveFields, []string{"api_key"}) || len(at.GuardedSettings) != 0 || !at.CustomUpstream() || len(at.DefaultModels) != 0 {
			t.Errorf("%s: sensitiveFields %v guardedSettings %v defaultModels %v", at.ID, at.SensitiveFields, at.GuardedSettings, at.DefaultModels)
		}
		assertJSON(t, at.Upstream.Layer(), declarations[at.ID])
	}
}

// TestRequests covers every route of both types: the URL on the account's
// resource endpoint, the key in api-key (never Authorization), the stream
// usage patch of chat, and the protocols a type does not serve.
func TestRequests(t *testing.T) {
	profiles := resolveAll(t)
	const deployment = "prod/gpt 4o" // a deployment name is escaped as one path segment
	for _, tc := range []struct {
		accountType, protocol string
		url                   string // "" = the type has no route for the protocol
	}{
		{typeV1, "openai.chat", resource + "/openai/v1/chat/completions"},
		{typeV1, "openai.responses", resource + "/openai/v1/responses"},
		{typeV1, "openai.embeddings", resource + "/openai/v1/embeddings"},
		{typeV1, "openai.responses_ws", ""},
		{typeDeployment, "openai.chat", resource + "/openai/deployments/prod%2Fgpt%204o/chat/completions?api-version=2025-04-01-preview"},
		{typeDeployment, "openai.embeddings", resource + "/openai/deployments/prod%2Fgpt%204o/embeddings?api-version=2025-04-01-preview"},
		{typeDeployment, "openai.responses", ""},
		{typeDeployment, "openai.responses_ws", ""},
	} {
		for _, stream := range []bool{false, true} {
			out, err := profiles[tc.accountType].Build(&pluginv1.BuildUpstreamRequestRequest{
				Meta:           &pluginv1.RequestMeta{Protocol: tc.protocol, Model: deployment, Stream: stream},
				Account:        account(tc.accountType, resource, `"api_version":"2025-04-01-preview"`),
				InboundHeaders: map[string]string{"user-agent": "cli/1"},
			})
			if tc.url == "" {
				if err == nil {
					t.Errorf("%s %s: built %s, want no route", tc.accountType, tc.protocol, out.Url)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: %v", tc.accountType, tc.protocol, err)
				continue
			}
			if out.Method != "POST" || out.Url != tc.url || out.UpstreamModel != deployment {
				t.Errorf("%s %s: %s %s model %q, want POST %s", tc.accountType, tc.protocol, out.Method, out.Url, out.UpstreamModel, tc.url)
			}
			wantHeaders := map[string]string{"api-key": apiKey, "content-type": "application/json", "user-agent": "cli/1"}
			if !reflect.DeepEqual(out.Headers, wantHeaders) {
				t.Errorf("%s %s: headers %v, want %v", tc.accountType, tc.protocol, out.Headers, wantHeaders)
			}
			wantPatch := stream && tc.protocol == "openai.chat"
			if got := len(out.Patches) == 1 && out.Patches[0].Path == "stream_options.include_usage" && out.Patches[0].ValueJson == "true"; got != wantPatch || len(out.Patches) > 1 {
				t.Errorf("%s %s stream=%v: patches %v", tc.accountType, tc.protocol, stream, out.Patches)
			}
		}
	}
}

// A classic route names the deployment in its path, so a request without a
// model is rejected rather than sent to /openai/deployments//...
func TestDeploymentModelRequired(t *testing.T) {
	_, err := resolveAll(t)[typeDeployment].Build(&pluginv1.BuildUpstreamRequestRequest{
		Meta:    &pluginv1.RequestMeta{Protocol: "openai.chat"},
		Account: account(typeDeployment, resource, `"api_version":"2024-10-21"`),
	})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("error %v, want model is required", err)
	}
}

// TestResourceEndpoint covers the account's base_url: it is required (there
// is no official address to fall back to), and a pasted /openai/v1 or
// /openai suffix is removed, when the account is saved and when it is used.
func TestResourceEndpoint(t *testing.T) {
	for accountType, p := range resolveAll(t) {
		for _, pasted := range []string{resource, resource + "/", resource + "/openai", resource + "/openai/v1", resource + "/openai/v1/"} {
			acc := account(accountType, pasted, `"api_version":"2024-10-21"`)
			res := p.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: accountType, CredentialsJson: acc.CredentialsJson, SettingsJson: acc.SettingsJson})
			var settings struct {
				BaseURL string `json:"base_url"`
			}
			_ = json.Unmarshal([]byte(res.NormalizedSettingsJson), &settings)
			if len(res.Errors) != 0 || settings.BaseURL != resource {
				t.Errorf("%s %s: errors %v, saved base_url %q, want %s", accountType, pasted, res.Errors, settings.BaseURL, resource)
			}
			out, err := p.Test(&pluginv1.BuildTestRequestRequest{Account: acc})
			if err != nil || !strings.HasPrefix(out.Url, resource+"/openai/") || strings.Contains(out.Url, "/openai/openai") {
				t.Errorf("%s %s: test request %s, %v", accountType, pasted, out.GetUrl(), err)
			}
		}

		acc := account(accountType, "", `"api_version":"2024-10-21"`)
		res := p.Validate(&pluginv1.ValidateCredentialsRequest{AccountType: accountType, CredentialsJson: acc.CredentialsJson, SettingsJson: acc.SettingsJson})
		if len(res.Errors) != 1 || res.Errors[0].Field != "base_url" || res.Errors[0].Code != "required" || res.Errors[0].Message != "Resource endpoint is required / 请填写资源地址" {
			t.Errorf("%s without base_url: validate errors %v", accountType, res.Errors)
		}
		if _, err := p.Build(&pluginv1.BuildUpstreamRequestRequest{Meta: &pluginv1.RequestMeta{Protocol: "openai.chat", Model: "d"}, Account: acc}); err == nil {
			t.Errorf("%s without base_url: built a request", accountType)
		}
	}
}

// TestAccountTest covers the console's account test (the chat route with
// the tested deployment, gpt-4o-mini when none is named) and the model
// listing the types do not have.
func TestAccountTest(t *testing.T) {
	for accountType, want := range map[string]map[string]string{
		typeV1:         {"": resource + "/openai/v1/chat/completions", "my-gpt": resource + "/openai/v1/chat/completions"},
		typeDeployment: {"": resource + "/openai/deployments/gpt-4o-mini/chat/completions?api-version=2024-10-21", "my-gpt": resource + "/openai/deployments/my-gpt/chat/completions?api-version=2024-10-21"},
	} {
		p, acc := resolveAll(t)[accountType], account(accountType, resource, `"api_version":"2024-10-21"`)
		for model, url := range want {
			out, err := p.Test(&pluginv1.BuildTestRequestRequest{Account: acc, Model: model})
			if err != nil {
				t.Fatalf("%s %q: %v", accountType, model, err)
			}
			wantHeaders := map[string]string{"api-key": apiKey, "content-type": "application/json"}
			if out.Method != "POST" || out.Url != url || !reflect.DeepEqual(out.Headers, wantHeaders) || out.UsageProtocol != "openai.chat" {
				t.Errorf("%s %q: %s %s %v usage %s, want POST %s", accountType, model, out.Method, out.Url, out.Headers, out.UsageProtocol, url)
			}
		}
		if _, err := p.Models(&pluginv1.BuildModelsRequestRequest{Account: acc}); err == nil {
			t.Errorf("%s: a model listing request was built", accountType)
		}
	}
}

// TestClassify pins §6.3: the openai policy as it is, with no Azure rules.
func TestClassify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const (
		toClient = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	p := resolveAll(t)[typeV1]
	for _, tc := range []struct {
		name    string
		status  int32
		headers map[string]string
		body    string
		action  pluginv1.ClassifyErrorResponse_Action
		effect  pluginv1.ClassifyErrorResponse_AccountEffect
		seconds int64 // cooldown from now
	}{
		{"content filter", 400, nil, `{"error":{"code":"content_filter","message":"The response was filtered"}}`, toClient, none, 0},
		{"bad request", 400, nil, `{"error":{"code":"BadRequest","message":"bad"}}`, toClient, none, 0},
		{"deployment not found", 404, nil, `{"error":{"code":"DeploymentNotFound","message":"no such deployment"}}`, toClient, none, 0},
		{"bad key", 401, nil, `{"error":{"code":"401","message":"Access denied due to invalid subscription key"}}`, failover, disable, 0},
		{"forbidden", 403, nil, `{"error":{"code":"403","message":"forbidden"}}`, failover, disable, 0},
		{"rate limit, retry-after-ms", 429, map[string]string{"retry-after-ms": "30000", "retry-after": "31"}, `{"error":{"code":"429","message":"slow down"}}`, failover, cooldown, 30},
		{"rate limit, retry-after", 429, map[string]string{"retry-after": "7"}, `{"error":{"code":"429","message":"slow down"}}`, failover, cooldown, 7},
		{"rate limit, no header", 429, nil, `{"error":{"code":"429","message":"slow down"}}`, failover, cooldown, 60},
		{"timeout", 408, nil, ``, failover, cooldown, 10},
		{"server error", 500, nil, `{"error":{"code":"InternalServerError","message":"oops"}}`, failover, cooldown, 10},
		{"unavailable", 503, nil, ``, failover, cooldown, 10},
	} {
		out := p.Classify(now, &pluginv1.ClassifyErrorRequest{
			Meta:   &pluginv1.RequestMeta{Protocol: "openai.chat"},
			Status: tc.status, Headers: tc.headers, BodyPrefix: []byte(tc.body),
		})
		until := int64(0)
		if tc.seconds > 0 {
			until = now.Unix() + tc.seconds
		}
		if out.Action != tc.action || out.AccountEffect != tc.effect || out.CooldownUntilUnix != until {
			t.Errorf("%s: %v %v until %d, want %v %v until %d", tc.name, out.Action, out.AccountEffect, out.CooldownUntilUnix, tc.action, tc.effect, until)
		}
	}
}

// TestForms pins the form rules the declarations rely on: the classic type
// always has an api-version (required, prefilled with the GA version), and
// both take a key without a vendor prefix.
func TestForms(t *testing.T) {
	type property struct {
		Default   string `json:"default"`
		Pattern   string `json:"pattern"`
		MinLength int    `json:"minLength"`
		MaxLength int    `json:"maxLength"`
	}
	type schema struct {
		Required             []string            `json:"required"`
		AdditionalProperties bool                `json:"additionalProperties"`
		Properties           map[string]property `json:"properties"`
	}
	for accountType, required := range map[string][]string{
		typeV1:         {"base_url", "api_key"},
		typeDeployment: {"base_url", "api_key", "api_version"},
	} {
		raw, err := os.ReadFile(path.Join("forms", accountType+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("%s: %v", accountType, err)
		}
		if !slices.Equal(s.Required, required) || s.AdditionalProperties || len(s.Properties) != len(required) {
			t.Errorf("%s: required %v, properties %v", accountType, s.Required, s.Properties)
		}
		if k := s.Properties["api_key"]; k.MinLength != 8 || k.MaxLength != 512 || !regexp.MustCompile(k.Pattern).MatchString(apiKey) {
			t.Errorf("%s: api_key %+v", accountType, k)
		}
	}
	raw, _ := os.ReadFile(path.Join("forms", typeDeployment+".schema.json"))
	var s schema
	_ = json.Unmarshal(raw, &s)
	v := s.Properties["api_version"]
	if v.Default != "2024-10-21" {
		t.Errorf("api_version default %q", v.Default)
	}
	re := regexp.MustCompile(v.Pattern)
	for value, ok := range map[string]bool{"2024-10-21": true, "2025-04-01-preview": true, "": false, "latest": false, "2024-10-21&x=1": false} {
		if re.MatchString(value) != ok {
			t.Errorf("api_version %q: match %v, want %v", value, !ok, ok)
		}
	}
}

// account is an account of accountType on baseURL ("" = not set) with
// extra settings members.
func account(accountType, baseURL string, settings ...string) *pluginv1.Account {
	if baseURL != "" {
		b, _ := json.Marshal(baseURL)
		settings = append(settings, `"base_url":`+string(b))
	}
	return &pluginv1.Account{
		Id: 1, Type: accountType,
		CredentialsJson: `{"api_key":"` + apiKey + `"}`,
		SettingsJson:    "{" + strings.Join(settings, ",") + "}",
	}
}

// resolveAll resolves the declarations of manifest.json with the client
// headers the gateway passes for the openai platform.
func resolveAll(t *testing.T) map[string]*upstream.Profile {
	t.Helper()
	out := map[string]*upstream.Profile{}
	for _, at := range load(t).AccountTypes {
		p, err := upstream.Resolve(&at, at.PassHeaders(builtinPlatform))
		if err != nil {
			t.Fatal(err)
		}
		out[at.ID] = p
	}
	return out
}

func builtinPlatform(id string) *manifest.Platform {
	for _, p := range platforms.Builtin() {
		if p.ID == id {
			return &p
		}
	}
	return nil
}

// load decodes manifest.json strictly and checks it with the files a
// package carries.
func load(t *testing.T) manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	files := map[string][]byte{"manifest.json": raw}
	for _, dir := range []string{"forms", "assets"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			if files[p], err = os.ReadFile(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := check.Validate(&m, files, check.ValidateOptions{Tooling: true}); err != nil {
		t.Fatal(err)
	}
	if w := check.Warnings(&m, files); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	return m
}

// assertJSON compares two JSON documents by value.
func assertJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal([]byte(want), &w)
	if g == nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("upstream = %s, want %s", got, want)
	}
}

package openai_test

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// azure_openai has no golden file: these tests are its acceptance
// (standard/goldentest/MANIFEST-ASSERTIONS.md, "azure_openai"), run against
// the two types as the azure_openai plugin builds them on the openai preset.

const (
	typeV1         = "azure_v1"
	typeDeployment = "azure_deployment"
	resource       = "https://res.openai.azure.com"
	azureKey       = "0123456789abcdef0123456789abcdef" // an Azure key has no prefix
)

// azure is what both types change in the openai preset: a required resource
// endpoint without a default (a pasted /openai/v1 or /openai removed), any
// key format, the key in api-key, no model listing, and the client headers
// of the openai platform without OpenAI-Organization and OpenAI-Project. The
// preset's filter fields stay.
func azure(id string, routes map[string]standard.Route) standard.Type {
	t := openai.APIKey(id)
	t.Spec.DefaultBaseURL, t.Spec.RequireBaseURL = "", true
	t.Spec.StripSuffixes = []string{"/openai/v1", "/openai"}
	t.Spec.KeyValidator = nil
	t.Spec.Texts.BaseURLRequired = "Resource endpoint is required / 请填写资源地址"
	t.Auth = standard.Auth{Header: "api-key"}
	t.PassHeaders = []string{"openai-beta", "user-agent", "x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version", "x-stainless-retry-count", "x-stainless-runtime", "x-stainless-runtime-version", "x-stainless-timeout"}
	t.Routes = routes
	t.Models = nil
	return t
}

// deployment is a route of the classic API: the deployment (the model) as
// one path segment, the account's api-version in the query.
func deployment(operation string, edit func([]byte, *standard.Request) ([]byte, error)) standard.Route {
	return standard.Route{
		Path: func(r *standard.Request) string {
			return "/openai/deployments/" + url.PathEscape(r.Model) + operation + "?api-version=" + url.QueryEscape(r.Field("api_version"))
		},
		Model: standard.Model{Required: true, Missing: "model is required (path /openai/deployments/{model}/...)"},
		Edit:  edit,
	}
}

func azurePlatforms() map[string]*standard.Platform {
	return map[string]*standard.Platform{
		typeV1: standard.New(azure(typeV1, map[string]standard.Route{
			openai.ProtocolChat:       {Path: standard.Path("/openai/v1/chat/completions"), Edit: openai.IncludeStreamUsage},
			openai.ProtocolResponses:  {Path: standard.Path("/openai/v1/responses")},
			openai.ProtocolEmbeddings: {Path: standard.Path("/openai/v1/embeddings")},
		})),
		typeDeployment: standard.New(azure(typeDeployment, map[string]standard.Route{
			openai.ProtocolChat:       deployment("/chat/completions", openai.IncludeStreamUsage),
			openai.ProtocolEmbeddings: deployment("/embeddings", nil),
		})),
	}
}

func azureAccount(accountType, baseURL string, settings ...string) *pluginv1.Account {
	if baseURL != "" {
		b, _ := json.Marshal(baseURL)
		settings = append(settings, `"base_url":`+string(b))
	}
	return &pluginv1.Account{Id: 1, Type: accountType, CredentialsJson: `{"api_key":"` + azureKey + `"}`, SettingsJson: "{" + strings.Join(settings, ",") + "}"}
}

func TestAzureRequests(t *testing.T) {
	platforms := azurePlatforms()
	const name = "prod/gpt 4o" // a deployment name is escaped as one path segment
	for _, tc := range []struct {
		accountType, protocol string
		url                   string // "" = the type has no route for the protocol
	}{
		{typeV1, openai.ProtocolChat, resource + "/openai/v1/chat/completions"},
		{typeV1, openai.ProtocolResponses, resource + "/openai/v1/responses"},
		{typeV1, openai.ProtocolEmbeddings, resource + "/openai/v1/embeddings"},
		{typeV1, openai.ProtocolResponsesWS, ""},
		{typeDeployment, openai.ProtocolChat, resource + "/openai/deployments/prod%2Fgpt%204o/chat/completions?api-version=2025-04-01-preview"},
		{typeDeployment, openai.ProtocolEmbeddings, resource + "/openai/deployments/prod%2Fgpt%204o/embeddings?api-version=2025-04-01-preview"},
		{typeDeployment, openai.ProtocolResponses, ""},
		{typeDeployment, openai.ProtocolResponsesWS, ""},
	} {
		for _, stream := range []bool{false, true} {
			body := `{"messages":[]}`
			out, err := platforms[tc.accountType].BuildUpstream(context.Background(), &pluginv1.ExecuteRequest{
				Meta:    &pluginv1.RequestMeta{Protocol: tc.protocol, Model: name, Stream: stream},
				Account: azureAccount(tc.accountType, resource, `"api_version":"2025-04-01-preview"`),
				InboundHeaders: map[string]string{
					"user-agent": "cli/1", "openai-beta": "assistants=v2", "x-stainless-lang": "python",
					"openai-organization": "org-client", "openai-project": "proj_client", "authorization": "Bearer sk-client",
				},
				Body: []byte(body),
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
			if out.Method != "POST" || out.Url != tc.url || out.UpstreamModel != name {
				t.Errorf("%s %s: %s %s model %q, want POST %s", tc.accountType, tc.protocol, out.Method, out.Url, out.UpstreamModel, tc.url)
			}
			wantDefault := map[string]string{"content-type": "application/json", "user-agent": "cli/1", "openai-beta": "assistants=v2", "x-stainless-lang": "python"}
			if !reflect.DeepEqual(out.Headers, map[string]string{"api-key": azureKey}) || !reflect.DeepEqual(out.DefaultHeaders, wantDefault) {
				t.Errorf("%s %s: headers %v default %v", tc.accountType, tc.protocol, out.Headers, out.DefaultHeaders)
			}
			if stream && tc.protocol == openai.ProtocolChat {
				body = `{"messages":[],"stream_options":{"include_usage":true}}`
			}
			if string(out.Body) != body {
				t.Errorf("%s %s stream=%v: body %s, want %s", tc.accountType, tc.protocol, stream, out.Body, body)
			}
		}
	}
}

// Both types filter the openai preset's fields, after the operator's
// rewrites (design §12.5): what reaches the plugin loses them.
func TestAzureBodyFilter(t *testing.T) {
	for accountType, p := range azurePlatforms() {
		out, err := p.BuildUpstream(context.Background(), &pluginv1.ExecuteRequest{
			Meta:    &pluginv1.RequestMeta{Protocol: openai.ProtocolEmbeddings, Model: "d"},
			Account: azureAccount(accountType, resource, `"api_version":"2024-10-21"`),
			Body:    []byte(`{"model":"d","service_tier":"priority","safety_identifier":"u1","user":"u1","messages":[]}`),
		})
		if err != nil || string(out.Body) != `{"model":"d","user":"u1","messages":[]}` {
			t.Errorf("%s: body %s, %v", accountType, out.GetBody(), err)
		}
	}
}

// A classic route names the deployment in its path, so a request without a
// model is refused rather than sent to /openai/deployments//...
func TestAzureDeploymentModelRequired(t *testing.T) {
	_, err := azurePlatforms()[typeDeployment].BuildUpstream(context.Background(), &pluginv1.ExecuteRequest{
		Meta:    &pluginv1.RequestMeta{Protocol: openai.ProtocolChat},
		Account: azureAccount(typeDeployment, resource, `"api_version":"2024-10-21"`),
		Body:    []byte(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("error %v, want model is required", err)
	}
}

// The resource endpoint is required and a pasted suffix is removed, when
// the account is saved and when it is used.
func TestAzureResourceEndpoint(t *testing.T) {
	const (
		cognitive = "https://res.cognitiveservices.azure.com"
		project   = "https://res.services.ai.azure.com/api/projects/p"
	)
	accepted := map[string]string{ // input -> the address requests are built on
		resource:                           resource,
		resource + "/":                     resource,
		resource + "/openai":               resource,
		resource + "/openai/":              resource,
		resource + "/openai/v1":            resource,
		resource + "/openai/v1/":           resource,
		cognitive:                          cognitive,
		cognitive + "/openai/v1":           cognitive,
		project:                            project,
		project + "/":                      project,
		"https://RES.OpenAI.Azure.com":     "https://RES.OpenAI.Azure.com",
		"http://mock-upstream:8080":        "http://mock-upstream:8080",
		"http://mock-upstream:8080/openai": "http://mock-upstream:8080",
	}
	ctx := context.Background()
	for accountType, p := range azurePlatforms() {
		for in, base := range accepted {
			acc := azureAccount(accountType, in, `"api_version":"2024-10-21"`)
			res, _ := p.ValidateCredentials(ctx, &pluginv1.ValidateCredentialsRequest{AccountType: accountType, CredentialsJson: acc.CredentialsJson, SettingsJson: acc.SettingsJson})
			var saved struct {
				BaseURL string `json:"base_url"`
			}
			_ = json.Unmarshal([]byte(res.NormalizedSettingsJson), &saved)
			out, err := p.BuildTestRequest(ctx, &pluginv1.BuildTestRequestRequest{Account: acc})
			if len(res.Errors) != 0 || saved.BaseURL != base || err != nil || !strings.HasPrefix(out.Url, base+"/openai/") || strings.Contains(out.Url, "/openai/openai") {
				t.Errorf("%s %q: errors %v, saved %q, test request %s (%v), want requests on %s", accountType, in, res.Errors, saved.BaseURL, out.GetUrl(), err, base)
			}
		}

		acc := azureAccount(accountType, "", `"api_version":"2024-10-21"`)
		res, _ := p.ValidateCredentials(ctx, &pluginv1.ValidateCredentialsRequest{AccountType: accountType, CredentialsJson: acc.CredentialsJson, SettingsJson: acc.SettingsJson})
		if len(res.Errors) != 1 || res.Errors[0].Field != "base_url" || res.Errors[0].Code != "required" || res.Errors[0].Message != "Resource endpoint is required / 请填写资源地址" {
			t.Errorf("%s without base_url: validate errors %v", accountType, res.Errors)
		}
		if _, err := p.BuildUpstream(ctx, &pluginv1.ExecuteRequest{Meta: &pluginv1.RequestMeta{Protocol: openai.ProtocolChat, Model: "d"}, Account: acc}); err == nil {
			t.Errorf("%s without base_url: built a request", accountType)
		}
	}
}

// The account test is the chat route with the tested deployment
// (gpt-4o-mini when none is named); there is no model listing.
func TestAzureAccountTest(t *testing.T) {
	ctx := context.Background()
	for accountType, want := range map[string]map[string]string{
		typeV1:         {"": resource + "/openai/v1/chat/completions", "my-gpt": resource + "/openai/v1/chat/completions"},
		typeDeployment: {"": resource + "/openai/deployments/gpt-4o-mini/chat/completions?api-version=2024-10-21", "my-gpt": resource + "/openai/deployments/my-gpt/chat/completions?api-version=2024-10-21"},
	} {
		p, acc := azurePlatforms()[accountType], azureAccount(accountType, resource, `"api_version":"2024-10-21"`)
		for model, u := range want {
			out, err := p.BuildTestRequest(ctx, &pluginv1.BuildTestRequestRequest{Account: acc, Model: model})
			if err != nil {
				t.Fatalf("%s %q: %v", accountType, model, err)
			}
			wantHeaders := map[string]string{"api-key": azureKey, "content-type": "application/json"}
			if out.Method != "POST" || out.Url != u || !reflect.DeepEqual(out.Headers, wantHeaders) || out.UsageProtocol != openai.ProtocolChat {
				t.Errorf("%s %q: %s %s %v usage %s, want POST %s", accountType, model, out.Method, out.Url, out.Headers, out.UsageProtocol, u)
			}
		}
		if _, err := p.BuildModelsRequest(ctx, &pluginv1.BuildModelsRequestRequest{Account: acc}); err == nil {
			t.Errorf("%s: a model listing request was built", accountType)
		}
	}
}

// The openai policy as it is, with no Azure rules.
func TestAzureClassify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const (
		toClient = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	p := azurePlatforms()[typeV1]
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
		out := p.ClassifyAt(now, &pluginv1.ClassifyErrorRequest{
			Meta:    &pluginv1.RequestMeta{Protocol: openai.ProtocolChat},
			Account: &pluginv1.Account{Type: typeV1},
			Status:  tc.status, Headers: tc.headers, BodyPrefix: []byte(tc.body),
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

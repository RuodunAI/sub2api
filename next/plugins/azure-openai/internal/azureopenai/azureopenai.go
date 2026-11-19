// Package azureopenai is the Azure OpenAI platform: two account types on
// the openai preset, the same resource endpoint and key served through the
// v1 API (azure_v1) or the classic per-deployment API (azure_deployment).
// They are two types because a route cannot depend on an account setting.
package azureopenai

import (
	"net/url"
	"slices"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/jsonedit"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// Account type ids (accountTypes[].id of manifest.json).
const (
	TypeV1         = "azure_v1"
	TypeDeployment = "azure_deployment"
)

// New returns the platform of both account types.
func New() *standard.Platform { return standard.New(V1(), Deployment()) }

// V1 is the azure_v1 type: the v1 API under <resource>/openai/v1.
func V1() standard.Type {
	return azure(TypeV1, map[string]standard.Route{
		openai.ProtocolChat:       {Path: standard.Path("/openai/v1/chat/completions"), Edit: openai.IncludeStreamUsage},
		openai.ProtocolResponses:  {Path: standard.Path("/openai/v1/responses")},
		openai.ProtocolEmbeddings: {Path: standard.Path("/openai/v1/embeddings")},
	})
}

// Deployment is the azure_deployment type: the classic API, chat
// completions and embeddings of a deployment.
func Deployment() standard.Type {
	return azure(TypeDeployment, map[string]standard.Route{
		openai.ProtocolChat:       deployment("/chat/completions", openai.IncludeStreamUsage),
		openai.ProtocolEmbeddings: deployment("/embeddings", nil),
	})
}

// azure is what both types change in the openai preset: a required resource
// endpoint without a default (a pasted /openai/v1 or /openai removed), any
// key format, the key in api-key, the routes given and the preset's tests
// on them, no model listing.
func azure(id string, routes map[string]standard.Route) standard.Type {
	t := openai.APIKey(id)
	t.Spec.DefaultBaseURL, t.Spec.RequireBaseURL = "", true
	t.Spec.StripSuffixes = []string{"/openai/v1", "/openai"}
	t.Spec.KeyValidator = nil
	t.Spec.Texts.BaseURLRequired = "Resource endpoint is required / 请填写资源地址"
	t.Auth = standard.Auth{Header: "api-key"}
	t.Routes = routes
	t.Tests = slices.DeleteFunc(t.Tests, func(x standard.Test) bool { _, ok := routes[x.Protocol]; return !ok })
	t.Models = nil
	// service_tier and safety_identifier belong to a client's own OpenAI
	// account. The plugin removes them after the operator's rewrites
	// (design §12.5), so an operator's param_override that sets either on
	// an Azure account is removed too.
	t.FilterFields = []string{"service_tier", "safety_identifier"}
	return t
}

// deployment is a route of the classic API: the deployment (the upstream
// model) as one path segment, the account's api_version in the query.
func deployment(operation string, edit func(*jsonedit.Doc, *standard.Request)) standard.Route {
	return standard.Route{
		Path: func(r *standard.Request) string {
			return "/openai/deployments/" + url.PathEscape(r.Model) + operation + "?api-version=" + url.QueryEscape(r.Field("api_version"))
		},
		Model: standard.Model{Required: true, Missing: "model is required (path /openai/deployments/{model}/...)"},
		Edit:  edit,
	}
}

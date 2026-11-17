// Package bedrock implements the bedrock plugin: three account types that
// reach models through the AWS Bedrock runtime
// (docs/DESIGN-2026-11-17-CLOUD-PROVIDERS.md §9). They serve two built-in
// platforms:
//
//   - anthropic (messages): Claude through InvokeModel and
//     InvokeModelWithResponseStream. The model goes into the path, the body
//     loses "model" and "stream" and gains anthropic_version, and a streamed
//     answer comes back as an AWS event stream the core decodes into SSE;
//   - openai (chat completions, responses): Bedrock's OpenAI-compatible
//     endpoints under /openai/v1, forwarded as they are.
//
// The types differ only in how a request is authorized:
//
//   - bedrock_apikey: a Bedrock API key, sent as a bearer token;
//   - bedrock_aksk: a long-term access key. The plugin hands the core a
//     signing directive and the core signs the final request (SigV4);
//   - bedrock_sts: temporary credentials, the same with a session token.
//     Nothing renews them: once they expire the upstream refuses the account
//     and an administrator replaces them.
//
// The plugin does no network I/O: it describes the request and the core
// sends it. It never sees the whole request body, so it cannot remove the
// fields Claude Code adds deep inside it (tool "custom", cache_control
// "scope" / "ttl"); the account's parameter override is the place for that.
//
// Never log an API key, a secret access key or a session token.
package bedrock

import (
	"strings"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
)

const (
	// Version is the plugin version (manifest.json "version").
	Version = "0.1.0"

	// Account types (manifest accountTypes[].id).
	TypeAPIKey    = "bedrock_apikey"
	TypeAccessKey = "bedrock_aksk"
	TypeSTS       = "bedrock_sts"

	// Upstream protocols BuildUpstreamRequest speaks: endpoints of the
	// built-in anthropic and openai platforms (sdk/platforms).
	ProtocolMessages  = anthropic.ProtocolMessages
	ProtocolChat      = "openai.chat"
	ProtocolResponses = "openai.responses"

	// DefaultRegion is the region of an account that names none.
	DefaultRegion = "us-east-1"
	// SigningService is the SigV4 service name of the Bedrock runtime.
	SigningService = "bedrock"
	// AnthropicVersion is the anthropic_version InvokeModel requires in a
	// Claude request body.
	AnthropicVersion = "bedrock-2023-05-31"
	// ResponseStream names the framing of a streamed InvokeModel answer
	// (BuildUpstreamRequestResponse.response_stream).
	ResponseStream = "aws-eventstream"

	// DefaultTestModel is the Bedrock model id BuildTestRequest invokes when
	// no model is given. The manifest's default mapping leads to it.
	DefaultTestModel = "anthropic.claude-haiku-4-5-20251001-v1:0"
)

// Endpoints are the addresses the plugin builds requests for. The zero value
// means the production address; tests point it at a fake server.
type Endpoints struct {
	// API is the scheme and host of every runtime request, without a
	// trailing slash ("" = https://bedrock-runtime.<region>.amazonaws.com).
	API string
}

// Plugin is the bedrock plugin. It implements pluginsdk.Platform. It lists
// no models: the console keeps the manifest defaults.
type Plugin struct {
	ep  Endpoints
	now func() time.Time
}

// New returns the plugin building requests for the production addresses.
func New() *Plugin { return NewWithEndpoints(Endpoints{}) }

// NewWithEndpoints returns the plugin building requests for ep.
func NewWithEndpoints(ep Endpoints) *Plugin {
	ep.API = strings.TrimRight(ep.API, "/")
	return &Plugin{ep: ep, now: time.Now}
}

// RuntimeHost is the Bedrock runtime host of a region.
func RuntimeHost(region string) string { return "bedrock-runtime." + region + ".amazonaws.com" }

// apiBase is the scheme and host serving region.
func (p *Plugin) apiBase(region string) string {
	if p.ep.API != "" {
		return p.ep.API
	}
	return "https://" + RuntimeHost(region)
}

package bedrock

import (
	"slices"
	"strings"

	"github.com/tidwall/gjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// InvokeModel takes beta features in the body (anthropic_beta), not in the
// anthropic-beta header, and refuses the ones it does not know. The lists
// below are those of the previous implementation
// (backend/internal/service/bedrock_request.go) plus the anthropic_beta
// values the AWS user guide names for InvokeModel on the bedrock-runtime
// endpoint (https://docs.aws.amazon.com/bedrock/latest/userguide/):
//
//   - compact-2026-09-04: the model cards of Opus 4.6 to Fable 5.1
//     (model-card-anthropic-claude-*.html, "Compaction");
//   - fallback-credit-2026-07-01: the model cards of Fable 5, Opus 5 and
//     Fable 5.1 ("Fallback credit for refused requests");
//   - thinking-display-updates-2026-08-18 and the four per-turn effort
//     values: claude-messages-adaptive-thinking.html.
//
// The list is not per model: a feature a model does not have is the
// upstream's to refuse. Unlike the previous implementation, the plugin never
// sees the body beyond its declared request fields, so it cannot add the
// features a body needs without naming them (computer use, tool search).

// betaHeader is the client header carrying the beta features.
const betaHeader = "anthropic-beta"

// Request body fields the beta features decide on.
const (
	// fieldBetas is where InvokeModel reads the beta features. A client
	// written for Bedrock sets it itself; the manifest declares it as a
	// request field, so the plugin is shown its value.
	fieldBetas = "anthropic_beta"
	// fieldContextManagement needs betaContextManagement.
	fieldContextManagement = "context_management"
)

const (
	betaContextManagement = "context-management-2025-06-27"
	betaToolSearch        = "tool-search-tool-2025-10-19"
	betaToolExamples      = "tool-examples-2025-10-29"
)

// supportedBetas are the beta features InvokeModel accepts.
var supportedBetas = []string{
	"computer-use-2025-01-24",
	"computer-use-2025-11-24",
	"context-1m-2025-08-07",
	betaContextManagement,
	"compact-2026-01-12",
	"compact-2026-09-04",
	"fallback-credit-2026-07-01",
	"thinking-display-updates-2026-08-18",
	"mid-conversation-output-config-2026-07-01",
	"mid-conversation-effort-2026-08-01",
	"per-turn-control-2026-07-01",
	"per-message-effort-2026-07-01",
	"fine-grained-tool-streaming-2025-05-14",
	betaToolSearch,
	betaToolExamples,
}

// betaAliases are features Bedrock knows under another name.
var betaAliases = map[string]string{
	"advanced-tool-use-2025-11-20": betaToolSearch,
}

// headerBetas are the features of an anthropic-beta header value.
func headerBetas(header string) []string {
	return strings.Split(header, ",")
}

// bodyBetas are the features of the body's anthropic_beta (its raw JSON, ""
// when the body has none): an array of names, or one string in the header's
// format. Anything else names no feature.
func bodyBetas(raw string) []string {
	value := gjson.Parse(raw)
	if value.Type == gjson.String {
		return headerBetas(value.Str)
	}
	var out []string
	for _, item := range value.Array() {
		if item.Type == gjson.String {
			out = append(out, item.Str)
		}
	}
	return out
}

// bedrockBetas are the features that go into anthropic_beta, out of those
// the client asked for in the header and then in the body: renamed where
// Bedrock has its own name, the unsupported ones dropped, each once, in the
// order given. Tool search brings tool examples with it.
func bedrockBetas(asked ...string) []string {
	var out []string
	for _, token := range asked {
		token = strings.TrimSpace(token)
		if alias, ok := betaAliases[token]; ok {
			token = alias
		}
		if slices.Contains(supportedBetas, token) && !slices.Contains(out, token) {
			out = append(out, token)
		}
	}
	if slices.Contains(out, betaToolSearch) && !slices.Contains(out, betaToolExamples) {
		out = append(out, betaToolExamples)
	}
	return out
}

// betaPatches settle the beta features of an InvokeModel body, given the
// client's header and the body's own anthropic_beta: the field becomes the
// features Bedrock accepts, or leaves the body when none remains, and a
// field that needs a feature the request does not have leaves with it.
func betaPatches(header, body string) []*pluginv1.BodyPatch {
	betas := bedrockBetas(append(headerBetas(header), bodyBetas(body)...)...)
	var patches []*pluginv1.BodyPatch
	switch {
	case len(betas) > 0:
		patches = append(patches, pluginsdk.SetPatch(fieldBetas, betas))
	case body != "":
		patches = append(patches, pluginsdk.DeletePatch(fieldBetas))
	}
	if !slices.Contains(betas, betaContextManagement) {
		patches = append(patches, pluginsdk.DeletePatch(fieldContextManagement))
	}
	return patches
}

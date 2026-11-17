package bedrock

import (
	"slices"
	"strings"
)

// InvokeModel takes beta features in the body (anthropic_beta), not in the
// anthropic-beta header, and refuses the ones it does not know. The lists
// below are those of the previous implementation
// (backend/internal/service/bedrock_request.go).

// betaHeader is the client header carrying the beta features.
const betaHeader = "anthropic-beta"

const (
	betaToolSearch   = "tool-search-tool-2025-10-19"
	betaToolExamples = "tool-examples-2025-10-29"
)

// supportedBetas are the beta features InvokeModel accepts.
var supportedBetas = []string{
	"computer-use-2025-01-24",
	"computer-use-2025-11-24",
	"context-1m-2025-08-07",
	"context-management-2025-06-27",
	"compact-2026-01-12",
	"fine-grained-tool-streaming-2025-05-14",
	betaToolSearch,
	betaToolExamples,
}

// betaAliases are features Bedrock knows under another name.
var betaAliases = map[string]string{
	"advanced-tool-use-2025-11-20": betaToolSearch,
}

// bedrockBetas are the features of an anthropic-beta header value that go
// into anthropic_beta: renamed where Bedrock has its own name, the
// unsupported ones dropped, each once, in the order given. Tool search
// brings tool examples with it.
func bedrockBetas(header string) []string {
	var out []string
	for _, token := range strings.Split(header, ",") {
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

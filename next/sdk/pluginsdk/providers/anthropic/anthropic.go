// Package anthropic is the Anthropic API key preset (APIKey) and what
// Anthropic-compatible plugins share: the protocol ids of the built-in
// platform, the test body and the error classification policy (parsing the
// error body, mapping statuses to client error types, computing rate-limit
// resets).
package anthropic

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/classify"
)

// StatusOverloaded is Anthropic's "overloaded" status code.
const StatusOverloaded = 529

// Protocol ids of the built-in anthropic platform (sdk/platforms/anthropic.json).
// They are the contract every Anthropic-compatible plugin writes its
// accountTypes[].platforms against, and the values RequestMeta.protocol
// carries for that platform's endpoints.
const (
	ProtocolMessages    = "anthropic.messages"
	ProtocolCountTokens = "anthropic.count_tokens"
)

// DefaultAPIVersion is sent when the client did not send anthropic-version.
const DefaultAPIVersion = "2023-06-01"

// MessagesPingBody is the body of a minimal Messages call: one user message
// asking the upstream to answer "ping". BuildTestRequest uses it so every
// Anthropic-compatible plugin probes the same shape; maxTokens stays a
// parameter because the minimum a model accepts differs (1 for Anthropic,
// 16 for some Ark models). The result is deterministic JSON: encoding/json
// sorts map keys.
func MessagesPingBody(model string, maxTokens int) string {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	return string(body)
}

// Policy is the shared Anthropic classify policy.
var Policy = classify.Policy{
	Parse:     parseError,
	ErrorType: errorTypeForStatus,
	Detail:    classify.DetailMessage,
	Rules: []classify.Rule{
		classify.EdgeBlock(),
		{When: classify.All(classify.Status(http.StatusBadRequest), billingError), Effect: classify.Disable, Reason: "upstream credit balance too low"},
		classify.Credentials(),
		classify.RateLimit(rateLimitReset),
		{When: classify.Status(StatusOverloaded), Effect: classify.Cooldown, Cooldown: 30 * time.Second, Reason: "upstream overloaded (529)"},
		classify.ServerErrors(10 * time.Second),
	},
}

func parseError(body []byte) classify.Error {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return classify.Error{}
	}
	return classify.Error{Message: gjson.GetBytes(body, "error.message").String()}
}

func errorTypeForStatus(in *classify.Input) string {
	switch in.Status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case StatusOverloaded:
		return "overloaded_error"
	case 0:
		return "api_error"
	default:
		if in.Status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}

func billingError(in *classify.Input) bool {
	m := strings.ToLower(in.Error.Message)
	return strings.Contains(m, "credit balance is too low")
}

// rateLimitReset works out when a 429'd account can be used again:
//  1. retry-after (seconds or HTTP date);
//  2. anthropic-ratelimit-*-reset of the exhausted limits (remaining == 0 or
//     status == rejected), the latest one;
//  3. otherwise the earliest future anthropic-ratelimit-*-reset;
//  4. otherwise nothing (the classify package defaults to 60s).
//
// Reset values are RFC 3339 timestamps or unix seconds.
func rateLimitReset(in *classify.Input) (time.Time, string, bool) {
	if t, src, ok := classify.RetryAfter(in); ok {
		return t, src, true
	}
	var exhausted, earliest time.Time
	for k, v := range in.Headers {
		if !strings.HasPrefix(k, "anthropic-ratelimit-") || !strings.HasSuffix(k, "-reset") {
			continue
		}
		t, ok := parseReset(v)
		if !ok {
			continue
		}
		base := strings.TrimSuffix(k, "-reset")
		if in.Headers[base+"-remaining"] == "0" || strings.EqualFold(in.Headers[base+"-status"], "rejected") {
			if t.After(exhausted) {
				exhausted = t
			}
		} else if t.After(in.Now) && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	if !exhausted.IsZero() {
		return exhausted, "anthropic-ratelimit-*-reset (exhausted)", true
	}
	if !earliest.IsZero() {
		return earliest, "anthropic-ratelimit-*-reset", true
	}
	return time.Time{}, "", false
}

func parseReset(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0), true
	}
	return time.Time{}, false
}

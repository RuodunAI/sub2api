package core

import (
	"net/http"
	"strings"
)

// ProviderHeaders are the provider response headers relayed to the client
// and shown by an account test: request ids, retry hints and the rate limit
// facts. Hop-by-hop headers and values with control characters are dropped;
// nothing else of the upstream response headers is copied.
func ProviderHeaders(source http.Header) http.Header {
	hop := map[string]bool{}
	for _, line := range source.Values("Connection") {
		for _, name := range strings.Split(line, ",") {
			hop[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	out := http.Header{}
	for name, values := range source {
		lower := strings.ToLower(name)
		allowed := lower == "request-id" || lower == "anthropic-request-id" || lower == "retry-after" || lower == "x-should-retry" ||
			strings.HasPrefix(lower, "anthropic-ratelimit-") || strings.HasPrefix(lower, "anthropic-fast-")
		if !allowed || hop[lower] {
			continue
		}
		for _, value := range values {
			if !strings.ContainsAny(value, "\r\n\x00") {
				out.Add(name, value)
			}
		}
	}
	return out
}

// ApplyProviderHeaders sets the ProviderHeaders of source on destination.
func ApplyProviderHeaders(destination, source http.Header) {
	for name, values := range ProviderHeaders(source) {
		destination[name] = values
	}
}

package protocol

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/http/httpguts"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// The method and header rules of a relayed request (UpstreamRequest,
// HostService.RelayUpstream). They are written once, here: the host's
// runtime refuses a relay that breaks them, the gateway checks the request
// it is about to send against them, and the SDK applies them in
// pluginsdk.Upstream.Build.

// RelayTransportHeaders are the request headers the host's transport
// decides for a relayed request; a relay naming one is refused. Host is the
// URL's: a request goes to, and names, the host that was checked (the SSRF
// policy, the account's proxy, the allowed upstream hosts).
var RelayTransportHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"accept-encoding":   true,
	"connection":        true,
	"transfer-encoding": true,
}

// ValidHeader reports whether name and value form a header net/http can
// send; one that is not would fail the exchange after it started.
func ValidHeader(name, value string) bool {
	return httpguts.ValidHeaderFieldName(name) && httpguts.ValidHeaderFieldValue(value)
}

// CheckRelayHeaders is why a relayed request's method or headers are
// refused, nil when they are not: the method is empty (POST) or an HTTP
// token; in headers and default_headers every name is lower-case, none is
// one of RelayTransportHeaders, and names and values are what HTTP can
// carry (no control character but a tab).
func CheckRelayHeaders(r *pluginv1.UpstreamRequest) error {
	var errs []error
	if m := r.GetMethod(); m != "" && !httpguts.ValidHeaderFieldName(m) {
		errs = append(errs, fmt.Errorf("upstream method %q is not an HTTP method", m))
	}
	for field, headers := range map[string]map[string]string{"headers": r.GetHeaders(), "default_headers": r.GetDefaultHeaders()} {
		for name, value := range headers {
			switch {
			case name == "" || name != strings.ToLower(name):
				errs = append(errs, fmt.Errorf("%s: header name %q must be lower-case", field, name))
			case RelayTransportHeaders[name]:
				errs = append(errs, fmt.Errorf("%s: header %q belongs to the host's transport and cannot be set", field, name))
			case !ValidHeader(name, value):
				errs = append(errs, fmt.Errorf("%s: header %q is malformed", field, name))
			}
		}
	}
	return errors.Join(errs...)
}

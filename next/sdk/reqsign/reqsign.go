// Package reqsign signs an outgoing HTTP request with a named scheme: callers
// hand over a Directive naming the scheme, and the package looks the scheme
// up in a table, so they hold no vendor logic. What a Directive can carry is
// not vendor-neutral, though: its fields are those of the access-key HMAC
// family (AWS SigV4 and the schemes shaped after it), see Directive. Only
// plugins sign: the upstream request they build in Execute, whose signed
// headers they hand to the host as authoritative headers
// (UpstreamRequest.headers), and the test request they describe.
//
// A scheme is signed by its vendor's official library where one exists (AWS
// SigV4: aws-sdk-go-v2's aws/signer/v4); the package decides what is signed
// and builds no service client. A wrong signature fails as a 403 from the
// vendor, not as a crash, so every scheme is pinned by golden vectors (see
// the tests of each scheme).
package reqsign

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// SchemeAWSSigV4 is AWS Signature Version 4 with header authentication.
const SchemeAWSSigV4 = "aws-sigv4"

// ErrUnknownScheme is returned (wrapped) by Sign for a scheme the package
// does not implement.
var ErrUnknownScheme = errors.New("reqsign: unknown signing scheme")

// Directive tells Sign how to sign one request. Only Scheme is generic: the
// other fields are the inputs of the access-key HMAC family (a key id and a
// secret, scoped to a region and a service, optionally with a session
// token). They are typed fields rather than an open map on purpose, so that
// the secret ones are known and can be redacted; a scheme of another family
// (an RSA key, a certificate) needs new fields here, not a reinterpretation
// of these.
//
// A Directive carries the secret and must stay out of logs and error
// messages; String, GoString and LogValue redact it for the cases where one
// is formatted by accident.
type Directive struct {
	Scheme          string
	Region          string
	Service         string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional
}

// scheme is one signing scheme: sign signs req in place (payload is the
// exact body req sends), headers are the headers a signature writes or
// covers, besides the host.
type scheme struct {
	sign    func(req *http.Request, payload []byte, d Directive, now time.Time) error
	headers []string
}

var schemes = map[string]scheme{
	SchemeAWSSigV4: {signAWSSigV4, sigV4SignedHeaders},
}

// Known reports whether Sign implements scheme.
func Known(scheme string) bool {
	_, ok := schemes[scheme]
	return ok
}

// Sign signs req in place. payload must be the exact bytes req sends as its
// body (nil or empty for none): it is hashed into the signature, which is why
// it is a parameter rather than read back from req.Body. Sign must run after
// the URL, the host and every header the scheme signs are final; headers the
// scheme does not sign may still change afterwards.
//
// An unknown scheme, or a directive the scheme cannot use, returns an error
// and leaves the request unsigned: the caller must not send it. Errors never
// contain the secret or the session token.
func Sign(req *http.Request, payload []byte, d Directive, now time.Time) error {
	s, ok := schemes[d.Scheme]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownScheme, d.Scheme)
	}
	if req == nil || req.URL == nil {
		return errors.New("reqsign: no request to sign")
	}
	return s.sign(req, payload, d, now)
}

// SignedHeaders returns the headers of req, signed by Sign with scheme,
// that the signature wrote or covers, those present: what a plugin hands
// to the host as authoritative headers so that no later layer changes
// them. The host is not among them (the URL carries it); every other
// header of req stays the plugin's to classify. For aws-sigv4:
// Authorization, X-Amz-Date, X-Amz-Security-Token and Content-Type. An
// unknown scheme has none.
func SignedHeaders(req *http.Request, scheme string) http.Header {
	out := http.Header{}
	for _, name := range schemes[scheme].headers {
		if values := req.Header.Values(name); len(values) > 0 {
			out[http.CanonicalHeaderKey(name)] = values
		}
	}
	return out
}

// String describes the directive without its secrets.
func (d Directive) String() string {
	return fmt.Sprintf("reqsign.Directive{scheme=%s region=%s service=%s access_key_id=%s session_token=%t}",
		d.Scheme, d.Region, d.Service, d.AccessKeyID, d.SessionToken != "")
}

// GoString keeps %#v from printing the secrets.
func (d Directive) GoString() string { return d.String() }

// LogValue keeps slog from printing the secrets.
func (d Directive) LogValue() slog.Value {
	return slog.GroupValue(slog.String("scheme", d.Scheme), slog.String("region", d.Region), slog.String("service", d.Service),
		slog.String("access_key_id", d.AccessKeyID), slog.Bool("session_token", d.SessionToken != ""))
}

// Package reqsign signs an outgoing HTTP request with a named scheme. It is
// provider-agnostic: callers hand over a Directive naming the scheme, and the
// package looks the scheme up in a table. The core signs the final upstream
// request with it (BuildUpstreamRequestResponse.signing in the plugin
// protocol), and a plugin signs the test request it builds itself.
//
// Only the standard library is used. A hand-written signature fails as a 403
// from the vendor, not as a crash, so every scheme is pinned by golden
// vectors (see the tests of each scheme).
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

// Directive tells Sign how to sign one request. It carries the secret and
// must stay out of logs and error messages; String, GoString and LogValue
// redact it for the cases where one is formatted by accident.
type Directive struct {
	Scheme          string
	Region          string
	Service         string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string // optional
}

// signer signs req in place for one scheme. payload is the exact body req
// sends.
type signer func(req *http.Request, payload []byte, d Directive, now time.Time) error

var schemes = map[string]signer{
	SchemeAWSSigV4: signAWSSigV4,
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
	sign, ok := schemes[d.Scheme]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownScheme, d.Scheme)
	}
	if req == nil || req.URL == nil {
		return errors.New("reqsign: no request to sign")
	}
	return sign(req, payload, d, now)
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

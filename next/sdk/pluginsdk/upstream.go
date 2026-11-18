package pluginsdk

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// transportHeaders belong to the host's transport: a request naming one is
// refused (host.proto, UpstreamRequest).
var transportHeaders = map[string]bool{
	"content-length":    true,
	"accept-encoding":   true,
	"connection":        true,
	"transfer-encoding": true,
}

// Upstream builds the final upstream request of one execution
// (pluginv1.UpstreamRequest). Header names are lower-cased for the caller;
// what the host would refuse is reported by Build.
//
// The two kinds of header differ in who has the last word:
//
//   - Header writes an AUTHORITATIVE header, applied after the operator's
//     passthrough and override, which therefore cannot change it. Only for
//     what must reach the upstream exactly as built: the credential header
//     and the headers a signature covers (with a signature, the Content-Type
//     it was computed with); a plugin whose released versions sent every
//     header above the operator's settings may keep doing so.
//   - DefaultHeader writes a DEFAULT header, applied before the operator's
//     settings: a passed through client header only fills a name left
//     unset, the account's header override and parameter override may
//     replace or remove it. Everything else belongs here.
//
// A name in both takes the authoritative value.
type Upstream struct {
	request *pluginv1.UpstreamRequest
	errs    []error
}

// NewUpstream starts a request. method "" is POST. rawURL is absolute,
// http(s) or, for a websocket turn, the URL of the handshake; write the path
// the way it goes on the wire, every segment already percent-encoded (the
// host parses it again before sending).
func NewUpstream(method, rawURL string) *Upstream {
	return &Upstream{request: &pluginv1.UpstreamRequest{Method: method, Url: rawURL}}
}

// Header sets an authoritative header. An empty value writes nothing.
func (u *Upstream) Header(name, value string) *Upstream {
	u.request.Headers = u.set(u.request.Headers, name, value)
	return u
}

// DefaultHeader sets a default header. An empty value writes nothing.
func (u *Upstream) DefaultHeader(name, value string) *Upstream {
	u.request.DefaultHeaders = u.set(u.request.DefaultHeaders, name, value)
	return u
}

// Headers sets every header of h as authoritative; several values of one
// name are joined with ", ". For a signed request pass only what the
// signature produced or covers (reqsign.SignedHeaders), not req.Header:
// every other header there would become authoritative too.
func (u *Upstream) Headers(h http.Header) *Upstream {
	for name, values := range h {
		u.Header(name, strings.Join(values, ", "))
	}
	return u
}

// DefaultHeaders sets every header of h as a default header.
func (u *Upstream) DefaultHeaders(h http.Header) *Upstream {
	for name, values := range h {
		u.DefaultHeader(name, strings.Join(values, ", "))
	}
	return u
}

// HeaderMap sets every header of h as authoritative.
func (u *Upstream) HeaderMap(h map[string]string) *Upstream {
	for name, value := range h {
		u.Header(name, value)
	}
	return u
}

// DefaultHeaderMap sets every header of h as a default header.
func (u *Upstream) DefaultHeaderMap(h map[string]string) *Upstream {
	for name, value := range h {
		u.DefaultHeader(name, value)
	}
	return u
}

func (u *Upstream) set(headers map[string]string, name, value string) map[string]string {
	if value == "" {
		return headers
	}
	if headers == nil {
		headers = map[string]string{}
	}
	headers[strings.ToLower(name)] = value
	return headers
}

// Body sets the final body bytes, for a websocket turn the message of the
// turn. The host sends them as they are: keeping the model reference and
// the stream flag of the body Execute was given is the plugin's part. The
// slice is not copied.
func (u *Upstream) Body(body []byte) *Upstream {
	u.request.Body = body
	return u
}

// Model records the upstream model after the plugin's own mapping in the
// usage log (optional).
func (u *Upstream) Model(model string) *Upstream {
	u.request.UpstreamModel = model
	return u
}

// ResponseStream names the format of an answer that is neither SSE nor
// JSON, e.g. awseventstream.FormatChunk; the host decodes it into
// server-sent events. Not on a websocket turn.
func (u *Upstream) ResponseStream(format string) *Upstream {
	u.request.ResponseStream = format
	return u
}

// ResponseHeaderTimeout shortens the time the host waits for the response
// headers. It can only tighten the account's effective value: a longer one
// changes nothing.
func (u *Upstream) ResponseHeaderTimeout(d time.Duration) *Upstream {
	u.options().ResponseHeaderTimeoutMs = u.millis("response header timeout", d)
	return u
}

// TotalTimeout shortens the time the whole exchange may take; like
// ResponseHeaderTimeout it can only tighten.
func (u *Upstream) TotalTimeout(d time.Duration) *Upstream {
	u.options().TotalTimeoutMs = u.millis("total timeout", d)
	return u
}

func (u *Upstream) options() *pluginv1.UpstreamOptions {
	if u.request.Options == nil {
		u.request.Options = &pluginv1.UpstreamOptions{}
	}
	return u.request.Options
}

// millis converts a timeout: a positive one is at least 1 ms, and one too
// long for the field is no tightening at all (0).
func (u *Upstream) millis(what string, d time.Duration) int32 {
	if d < 0 {
		u.errs = append(u.errs, fmt.Errorf("%s is negative", what))
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms > math.MaxInt32 {
		return 0
	}
	return int32(ms)
}

// Build returns the request, or what makes the host refuse it as an
// INVALID_ARGUMENT error.
func (u *Upstream) Build() (*pluginv1.UpstreamRequest, error) {
	if err := errors.Join(append(u.errs, ValidateUpstreamRequest(u.request))...); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return u.request, nil
}

// ValidateUpstreamRequest checks what the host refuses in a relayed request
// without looking at the account: the URL is absolute http(s) (ws(s) for a
// websocket handshake), header names are lower-case and none is the
// transport's, values hold no line break, options are not negative. Relay
// applies it before the host is asked.
func ValidateUpstreamRequest(r *pluginv1.UpstreamRequest) error {
	if r == nil {
		return errors.New("upstream request is required")
	}
	var errs []error
	if err := checkURL(r.GetUrl(), "http", "https", "ws", "wss"); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, checkHeaders("headers", r.GetHeaders()), checkHeaders("default_headers", r.GetDefaultHeaders()), checkOptions(r.GetOptions()))
	return errors.Join(errs...)
}

func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream url: %w", err)
	}
	for _, scheme := range schemes {
		if u.Scheme == scheme && u.Host != "" {
			return nil
		}
	}
	return fmt.Errorf("upstream url %q is not an absolute %s URL", raw, strings.Join(schemes, "/"))
}

func checkHeaders(field string, headers map[string]string) error {
	var errs []error
	for name, value := range headers {
		switch {
		case name == "" || name != strings.ToLower(name):
			errs = append(errs, fmt.Errorf("%s: header name %q must be lower-case", field, name))
		case transportHeaders[name]:
			errs = append(errs, fmt.Errorf("%s: header %q belongs to the host's transport and cannot be set", field, name))
		case strings.ContainsAny(name, " \r\n:") || strings.ContainsAny(value, "\r\n"):
			errs = append(errs, fmt.Errorf("%s: header %q is malformed", field, name))
		}
	}
	return errors.Join(errs...)
}

func checkOptions(o *pluginv1.UpstreamOptions) error {
	if o.GetResponseHeaderTimeoutMs() < 0 || o.GetTotalTimeoutMs() < 0 {
		return errors.New("upstream options: a timeout is negative")
	}
	return nil
}

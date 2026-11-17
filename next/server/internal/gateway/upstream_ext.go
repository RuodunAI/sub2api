package gateway

// Two optional parts of a plugin-built upstream request that the core
// carries out, each looked up by name with no vendor logic here:
//
//   - signing: the final request is signed right before it is sent
//     (sdk/reqsign). Only the core can: the signature covers the body, which
//     a plugin never sees whole and which is still rewritten after the
//     plugin answered (patches, forced stream flag).
//   - response_stream: a response body in a framing other than SSE or JSON
//     is decoded into SSE (sdk/awseventstream), after which relaying,
//     metering and conversion handle it like any event stream.
//
// A request without either field takes neither path.

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/awseventstream"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// responseStream is a response framing the core turns into SSE.
type responseStream struct {
	// contentType is the media type the upstream answers with.
	contentType string
	// sse returns the SSE form of a body in this framing.
	sse func(io.Reader) io.Reader
}

// responseStreams maps BuildUpstreamRequestResponse.response_stream names.
var responseStreams = map[string]responseStream{
	"aws-eventstream": {
		contentType: awseventstream.ContentType,
		sse:         func(r io.Reader) io.Reader { return awseventstream.NewSSEReader(r) },
	},
}

// checkUpstreamExt reports what built asks of the core that this core cannot
// do. It runs before anything is sent: a request that should have been
// signed must not go out unsigned, and an answer in an unknown framing could
// not be relayed.
func checkUpstreamExt(built *pluginv1.BuildUpstreamRequestResponse) error {
	if s := built.GetSigning(); s != nil && !reqsign.Known(s.GetScheme()) {
		return fmt.Errorf("%w: %q", reqsign.ErrUnknownScheme, s.GetScheme())
	}
	if name := built.GetResponseStream(); name != "" {
		if _, ok := responseStreams[name]; !ok {
			return fmt.Errorf("unknown response stream format %q", name)
		}
	}
	return nil
}

// applyUpstreamExt carries out built's part before the send: it signs req,
// whose URL, host and headers are final, over body, the bytes it sends. A
// failure fails the attempt as plugin-unavailable; nil means the request may
// go out. The errors name the scheme or the format, never a credential.
func applyUpstreamExt(req *http.Request, body []byte, built *pluginv1.BuildUpstreamRequestResponse) *attemptResult {
	err := checkUpstreamExt(built)
	if s := built.GetSigning(); err == nil && s != nil {
		if req.ContentLength == 0 {
			body = nil // a request without a body signs the empty payload
		}
		err = reqsign.Sign(req, body, reqsign.Directive{Scheme: s.GetScheme(), Region: s.GetRegion(), Service: s.GetService(),
			AccessKeyID: s.GetAccessKeyId(), SecretAccessKey: s.GetSecretAccessKey(), SessionToken: s.GetSessionToken()}, time.Now())
	}
	if err != nil {
		return &attemptResult{kind: attemptFailover, err: fromCore(core.ErrPluginUnavailable.WithCause(err), errTypePluginUnavailable)}
	}
	return nil
}

// adaptResponseStream presents a successful answer in the framing the plugin
// named as an event stream. An answer with another content type (a JSON
// body, plain SSE) is left as it is.
func adaptResponseStream(resp *http.Response, name string) {
	format, ok := responseStreams[name]
	if !ok {
		return
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct != format.contentType {
		return
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{format.sse(resp.Body), resp.Body}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
}

// unsupportedOnWebSocket fails a handshake whose plugin asked for signing or
// a response framing: neither is carried out on a WebSocket, and the
// handshake must not go out unsigned.
func unsupportedOnWebSocket(built *pluginv1.BuildUpstreamRequestResponse) *attemptResult {
	if built.GetSigning() == nil && built.GetResponseStream() == "" {
		return nil
	}
	err := errors.New("request signing and response stream formats are not supported on WebSocket routes")
	return &attemptResult{kind: attemptFailover, err: fromCore(core.ErrPluginUnavailable.WithCause(err), errTypePluginUnavailable)}
}

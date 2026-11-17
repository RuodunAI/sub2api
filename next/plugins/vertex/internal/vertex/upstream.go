package vertex

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
)

// actions are the model methods of the upstream protocols. Streams are asked
// for as server-sent events, the only stream format the core relays.
var actions = map[string]string{
	ProtocolGenerate:       "generateContent",
	ProtocolStreamGenerate: "streamGenerateContent?alt=sse",
	ProtocolCountTokens:    "countTokens",
}

// modelPrefix is the resource prefix clients may put before a model id.
const modelPrefix = "models/"

// apiBase is the scheme and host serving t: the global host for the express
// address and the global location, else the location's own.
func (p *Plugin) apiBase(t target) string {
	switch {
	case p.ep.API != "":
		return p.ep.API
	case t.express() || t.Location == LocationGlobal:
		return "https://" + APIHost
	}
	return "https://" + t.Location + "-" + APIHost
}

// modelURL is the address of action on a Google publisher model, and the
// model id it names. With project and location it is the full resource path;
// without (an API key's express address) the publisher path alone.
func (p *Plugin) modelURL(t target, model, action string) (address, id string, err error) {
	id = strings.TrimPrefix(strings.TrimSpace(model), modelPrefix)
	if id == "" {
		return "", "", status.Error(codes.InvalidArgument, "the request names no model")
	}
	path := "/v1"
	if !t.express() {
		path += "/projects/" + url.PathEscape(t.Project) + "/locations/" + url.PathEscape(t.Location)
	}
	return p.apiBase(t) + path + "/publishers/google/models/" + url.PathEscape(id) + ":" + action, id, nil
}

// headers are the upstream headers of an account's requests at now: the API
// key, or the access token the core keeps renewing. An account without a
// usable token (none fetched yet, or the stored one has expired) answers
// FAILED_PRECONDITION; the gateway moves on to another account without
// penalizing this one.
func (a account) headers(now time.Time) (map[string]string, error) {
	h := map[string]string{"content-type": "application/json"}
	if a.typ == TypeAPIKey {
		key := a.creds.Str(fieldAPIKey)
		if key == "" {
			return nil, status.Error(codes.FailedPrecondition, "the account has no API key / 账号没有 API Key")
		}
		h["x-goog-api-key"] = key
		return h, nil
	}
	token := a.creds.Str(fieldAccessToken)
	if token == "" || a.tokenExpired(now) {
		return nil, status.Error(codes.FailedPrecondition, "the account has no valid access token (none yet, or expired); the next credential refresh fetches one / 账号没有有效的访问令牌（尚未获取或已过期），等待下一次凭证刷新")
	}
	h["authorization"] = "Bearer " + token
	if keyKinds[a.typ].quotaProject {
		h["x-goog-user-project"] = a.target.Project
	}
	return h, nil
}

// tokenExpired reports a stored access token whose expires_at has passed. A
// token without a readable expiry is not known to be expired.
func (a account) tokenExpired(now time.Time) bool {
	at, ok := expiry.ExpiresAt(a.creds)
	return ok && !at.After(now)
}

// upstreamRequest is a POST of an action on a model for an account.
type upstreamRequest struct {
	url string
	// model is the model id the URL names.
	model   string
	headers map[string]string
}

// request describes a POST of action on model for the account.
func (p *Plugin) request(acc *pluginv1.Account, model, action string) (upstreamRequest, error) {
	a, err := readAccount(acc)
	if err != nil {
		return upstreamRequest{}, err
	}
	headers, err := a.headers(p.now())
	if err != nil {
		return upstreamRequest{}, err
	}
	address, id, err := p.modelURL(a.target, model, action)
	return upstreamRequest{url: address, model: id, headers: headers}, err
}

// BuildUpstreamRequest implements pluginsdk.Platform. The body is the Gemini
// API's and is forwarded unchanged; the model (after the account's mapping)
// goes into the path.
func (p *Plugin) BuildUpstreamRequest(_ context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	protocol := in.GetMeta().GetProtocol()
	action, ok := actions[protocol]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", protocol)
	}
	req, err := p.request(in.GetAccount(), in.GetMeta().GetModel(), action)
	if err != nil {
		return nil, err
	}
	return &pluginv1.BuildUpstreamRequestResponse{Method: http.MethodPost, Url: req.url, Headers: req.headers, UpstreamModel: req.model}, nil
}

// BuildTestRequest implements pluginsdk.Platform: the smallest
// generateContent call. Its usage is read with the gemini.generate rules.
func (p *Plugin) BuildTestRequest(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	model := strings.TrimSpace(in.GetModel())
	if model == "" {
		model = DefaultTestModel
	}
	req, err := p.request(in.GetAccount(), model, actions[ProtocolGenerate])
	if err != nil {
		return nil, err
	}
	return &pluginv1.BuildTestRequestResponse{
		Method:        http.MethodPost,
		Url:           req.url,
		Headers:       req.headers,
		BodyJson:      gemini.GenerateContentPingBody(1),
		Model:         req.model,
		UsageProtocol: ProtocolGenerate,
	}, nil
}

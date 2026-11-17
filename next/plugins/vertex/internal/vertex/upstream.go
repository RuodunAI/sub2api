package vertex

import (
	"context"
	"net/http"
	"net/url"
	"strings"

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

// headers are the upstream headers of an account's requests: the API key, or
// the access token the core keeps renewing. An account still waiting for its
// first token answers FAILED_PRECONDITION; the gateway moves on to another
// account without penalizing this one.
func (a account) headers() (map[string]string, error) {
	h := map[string]string{"content-type": "application/json"}
	if a.typ == TypeAPIKey {
		key := a.creds.str(fieldAPIKey)
		if key == "" {
			return nil, status.Error(codes.FailedPrecondition, "the account has no API key / 账号没有 API Key")
		}
		h["x-goog-api-key"] = key
		return h, nil
	}
	token := a.creds.str(fieldAccessToken)
	if token == "" {
		return nil, status.Error(codes.FailedPrecondition, "the account has no access token yet; it is fetched by the next credential refresh / 账号还没有访问令牌，等待下一次凭证刷新")
	}
	h["authorization"] = "Bearer " + token
	return h, nil
}

// request describes a POST of action on model for the account.
func (p *Plugin) request(acc *pluginv1.Account, model, action string) (address, id string, headers map[string]string, err error) {
	a, err := readAccount(acc)
	if err != nil {
		return "", "", nil, err
	}
	if headers, err = a.headers(); err != nil {
		return "", "", nil, err
	}
	address, id, err = p.modelURL(a.target, model, action)
	return address, id, headers, err
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
	address, model, headers, err := p.request(in.GetAccount(), in.GetMeta().GetModel(), action)
	if err != nil {
		return nil, err
	}
	return &pluginv1.BuildUpstreamRequestResponse{Method: http.MethodPost, Url: address, Headers: headers, UpstreamModel: model}, nil
}

// BuildTestRequest implements pluginsdk.Platform: the smallest
// generateContent call. Its usage is read with the gemini.generate rules.
func (p *Plugin) BuildTestRequest(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	model := strings.TrimSpace(in.GetModel())
	if model == "" {
		model = DefaultTestModel
	}
	address, model, headers, err := p.request(in.GetAccount(), model, actions[ProtocolGenerate])
	if err != nil {
		return nil, err
	}
	return &pluginv1.BuildTestRequestResponse{
		Method:        http.MethodPost,
		Url:           address,
		Headers:       headers,
		BodyJson:      gemini.GenerateContentPingBody(1),
		Model:         model,
		UsageProtocol: ProtocolGenerate,
	}, nil
}

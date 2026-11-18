package bedrock

import (
	"context"
	"maps"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
)

// contentJSON is the content type of an InvokeModel request and of the
// answer asked for (a streamed answer is an event stream whatever Accept
// says).
const contentJSON = "application/json"

// BuildUpstream implements pluginsdk.UpstreamBuilder: a Claude request for
// InvokeModel, or InvokeModelWithResponseStream when it streams, with the
// final InvokeModel body (invokeBody), authorized by the account.
func (p *Plugin) BuildUpstream(_ context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	if protocol := in.GetMeta().GetProtocol(); protocol != ProtocolMessages {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", protocol)
	}
	a, err := readAccount(in.GetAccount())
	if err != nil {
		return nil, err
	}
	id, err := a.modelID(pluginsdk.RequestModel(in))
	if err != nil {
		return nil, err
	}
	body, err := invokeBody(in.GetBody(), in.GetInboundHeaders()[betaHeader], id)
	if err != nil {
		return nil, err
	}
	return p.invoke(a, id, pluginsdk.RequestStream(in), body)
}

// invoke is the InvokeModel request of the account for model id with the
// final body. Content-Type and Accept are defaults the operator may change;
// the authorization is authoritative, and with a signature so is the
// Content-Type it covers. The URL is the one signed.
func (p *Plugin) invoke(a account, id string, stream bool, body []byte) (*pluginv1.UpstreamRequest, error) {
	req, err := http.NewRequest(http.MethodPost, p.apiBase(a.region)+invokePath(id, stream), nil)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "the request has no valid address")
	}
	req.Header.Set("Content-Type", contentJSON)
	auth, err := a.authorize(req, body, p.now())
	if err != nil {
		return nil, err
	}
	u := pluginsdk.NewUpstream(req.Method, req.URL.String()).
		DefaultHeader("content-type", contentJSON).
		DefaultHeader("accept", contentJSON).
		Headers(auth).
		Body(body).
		Model(id)
	if stream {
		u.ResponseStream(ResponseStream)
	}
	built, err := u.Build()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return built, nil
}

// BuildTestRequest implements pluginsdk.Platform: a non-streaming
// InvokeModel call of the Messages ping. The host sends it as described, so
// the plugin signs it like a gateway request. Its usage is read with the
// anthropic.messages rules.
func (p *Plugin) BuildTestRequest(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	a, err := readAccount(in.GetAccount())
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(in.GetModel())
	if model == "" {
		model = DefaultTestModel
	}
	id, err := a.modelID(model)
	if err != nil {
		return nil, err
	}
	body, err := invokeBody([]byte(anthropic.MessagesPingBody("", 1)), "", id)
	if err != nil {
		return nil, err
	}
	up, err := p.invoke(a, id, false, body)
	if err != nil {
		return nil, err
	}
	headers := maps.Clone(up.GetDefaultHeaders())
	maps.Copy(headers, up.GetHeaders())
	return &pluginv1.BuildTestRequestResponse{
		Method: up.GetMethod(), Url: up.GetUrl(), Headers: headers, BodyJson: string(up.GetBody()),
		Model: id, UsageProtocol: ProtocolMessages,
	}, nil
}

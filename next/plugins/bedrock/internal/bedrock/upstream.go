package bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/sjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
)

// route builds the request of one upstream protocol for an account, without
// its host and its credential: Url holds the path.
type route func(a account, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error)

var routes = map[string]route{
	ProtocolMessages:  invokeModel,
	ProtocolChat:      openAICompatible("/openai/v1/chat/completions", setPatch("stream_options.include_usage", true)),
	ProtocolResponses: openAICompatible("/openai/v1/responses"),
}

func setPatch(path string, value any) *pluginv1.BodyPatch {
	raw, _ := json.Marshal(value)
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_SET, Path: path, ValueJson: string(raw)}
}

func deletePatch(path string) *pluginv1.BodyPatch {
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_DELETE, Path: path}
}

// invokeHeaders are the headers of an InvokeModel request.
func invokeHeaders() map[string]string {
	return map[string]string{"content-type": "application/json", "accept": "application/json"}
}

// invokeModel builds a Claude request for InvokeModel. The path names the
// model and whether the answer streams, so the body loses both fields; the
// client's beta features move from the header into the body.
func invokeModel(a account, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	meta := in.GetMeta()
	id, err := a.modelID(meta.GetModel())
	if err != nil {
		return nil, err
	}
	resp := &pluginv1.BuildUpstreamRequestResponse{
		Url:           invokePath(id, meta.GetStream()),
		Headers:       invokeHeaders(),
		Patches:       []*pluginv1.BodyPatch{deletePatch("model"), deletePatch("stream"), setPatch("anthropic_version", AnthropicVersion)},
		UpstreamModel: id,
	}
	if betas := bedrockBetas(in.GetInboundHeaders()[betaHeader]); len(betas) > 0 {
		resp.Patches = append(resp.Patches, setPatch("anthropic_beta", betas))
	}
	if meta.GetStream() {
		resp.ResponseStream = ResponseStream
	}
	return resp, nil
}

// openAICompatible builds requests for one of Bedrock's OpenAI-compatible
// endpoints. The body is the client's (the model stays in it) and the answer
// is plain SSE or JSON; streamPatches apply to streamed requests only.
func openAICompatible(path string, streamPatches ...*pluginv1.BodyPatch) route {
	return func(_ account, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
		resp := &pluginv1.BuildUpstreamRequestResponse{
			Url:           path,
			Headers:       map[string]string{"content-type": "application/json"},
			UpstreamModel: in.GetMeta().GetModel(),
		}
		if in.GetMeta().GetStream() {
			resp.Patches = streamPatches
		}
		return resp, nil
	}
}

// BuildUpstreamRequest implements pluginsdk.Platform. An API key account
// sends its key; for the other types the answer carries a signing directive
// and the core signs the final request.
func (p *Plugin) BuildUpstreamRequest(_ context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	protocol := in.GetMeta().GetProtocol()
	build, ok := routes[protocol]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", protocol)
	}
	a, err := readAccount(in.GetAccount())
	if err != nil {
		return nil, err
	}
	resp, err := build(a, in)
	if err != nil {
		return nil, err
	}
	resp.Method = http.MethodPost
	resp.Url = p.apiBase(a.region) + resp.Url
	if !a.auth.signed {
		resp.Headers["authorization"] = a.bearer()
		return resp, nil
	}
	d := a.directive()
	resp.Signing = &pluginv1.RequestSigning{
		Scheme: d.Scheme, Region: d.Region, Service: d.Service,
		AccessKeyId: d.AccessKeyID, SecretAccessKey: d.SecretAccessKey, SessionToken: d.SessionToken,
	}
	return resp, nil
}

// testBody is the smallest InvokeModel call: the Messages ping without its
// model, with the version Bedrock requires.
func testBody() string {
	body, _ := sjson.Delete(anthropic.MessagesPingBody("", 1), "model")
	body, _ = sjson.Set(body, "anthropic_version", AnthropicVersion)
	return body
}

// signedHeaders are the headers the signature of a request adds.
var signedHeaders = []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"}

// sign adds the SigV4 headers of a POST of body to address with headers. The
// host sends a test request exactly as described, so the plugin, which wrote
// the body, signs it itself.
func (p *Plugin) sign(address string, headers map[string]string, body string, d reqsign.Directive) error {
	req, err := http.NewRequest(http.MethodPost, address, nil)
	if err != nil {
		return status.Error(codes.Internal, "the test request has no valid address")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if err := reqsign.Sign(req, []byte(body), d, p.now()); err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	for _, name := range signedHeaders {
		if value := req.Header.Get(name); value != "" {
			headers[strings.ToLower(name)] = value
		}
	}
	return nil
}

// BuildTestRequest implements pluginsdk.Platform: a non-streaming
// InvokeModel call. Its usage is read with the anthropic.messages rules.
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
	resp := &pluginv1.BuildTestRequestResponse{
		Method:        http.MethodPost,
		Url:           p.apiBase(a.region) + invokePath(id, false),
		Headers:       invokeHeaders(),
		BodyJson:      testBody(),
		Model:         id,
		UsageProtocol: ProtocolMessages,
	}
	if !a.auth.signed {
		resp.Headers["authorization"] = a.bearer()
		return resp, nil
	}
	if err := p.sign(resp.Url, resp.Headers, resp.BodyJson, a.directive()); err != nil {
		return nil, err
	}
	return resp, nil
}

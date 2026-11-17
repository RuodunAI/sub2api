package bedrock

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/sjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/reqsign"
)

// invokeHeaders are the headers of an InvokeModel request.
func invokeHeaders() map[string]string {
	return map[string]string{"content-type": "application/json", "accept": "application/json"}
}

// droppedFields are top-level fields of Anthropic's own API that InvokeModel
// has no use for. The previous implementation removed output_config and
// output_format as well; they stay, because InvokeModel now reads the output
// format and the effort from output_config (AWS user guide: "Get validated
// JSON results from models", "Adaptive thinking") and output_format is its
// deprecated spelling.
var droppedFields = []string{"metadata", "provider"}

// invokePatches turn a Messages body into an InvokeModel one. The path names
// the model and whether the answer streams, so the body loses both fields;
// it gains the version Bedrock requires, loses droppedFields, and the
// client's beta features move from the header into the body.
func invokePatches(in *pluginv1.BuildUpstreamRequestRequest) []*pluginv1.BodyPatch {
	patches := []*pluginv1.BodyPatch{
		pluginsdk.DeletePatch("model"), pluginsdk.DeletePatch("stream"),
		pluginsdk.SetPatch("anthropic_version", AnthropicVersion),
	}
	for _, field := range droppedFields {
		patches = append(patches, pluginsdk.DeletePatch(field))
	}
	return append(patches, betaPatches(in.GetInboundHeaders()[betaHeader], in.GetFields()[fieldBetas])...)
}

// signing is a directive as the core reads it.
func signing(d reqsign.Directive) *pluginv1.RequestSigning {
	return &pluginv1.RequestSigning{
		Scheme: d.Scheme, Region: d.Region, Service: d.Service,
		AccessKeyId: d.AccessKeyID, SecretAccessKey: d.SecretAccessKey, SessionToken: d.SessionToken,
	}
}

// BuildUpstreamRequest implements pluginsdk.Platform: a Claude request for
// InvokeModel, or InvokeModelWithResponseStream when it streams. An API key
// account sends its key; for the other types the answer carries a signing
// directive and the core signs the final request.
func (p *Plugin) BuildUpstreamRequest(_ context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	meta := in.GetMeta()
	if protocol := meta.GetProtocol(); protocol != ProtocolMessages {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported upstream protocol %q", protocol)
	}
	a, err := readAccount(in.GetAccount())
	if err != nil {
		return nil, err
	}
	id, err := a.modelID(meta.GetModel())
	if err != nil {
		return nil, err
	}
	resp := &pluginv1.BuildUpstreamRequestResponse{
		Method:        http.MethodPost,
		Url:           p.apiBase(a.region) + invokePath(id, meta.GetStream()),
		Headers:       invokeHeaders(),
		Patches:       invokePatches(in),
		UpstreamModel: id,
	}
	if meta.GetStream() {
		resp.ResponseStream = ResponseStream
	}
	if d, signed := a.authorize(resp.Headers); signed {
		resp.Signing = signing(d)
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
	if d, signed := a.authorize(resp.Headers); signed {
		if err := p.sign(resp.Url, resp.Headers, resp.BodyJson, d); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

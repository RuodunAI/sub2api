// Package declarative serves the account types that declare their upstream
// (manifest accountTypes[].upstream, docs/UPSTREAM-DECLARATIVE.md §4.1): it
// implements core.PlatformPlugin locally with the sdk/upstream engine, so
// credential validation, request building, error classification and the
// test and model listing requests need no plugin process. In extend mode the
// plugin still amends each built request.
package declarative

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/upstream"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// platform names the embedded fallback of Client: an alias keeps the
// embedded field unexported.
type platform = core.PlatformPlugin

// Client is the core.PlatformPlugin of one declared account type. The five
// entry points the declaration covers come from its profile; every other
// one (quota, refresh, reconcile, and the optional account lifecycle and
// model availability) goes to the declaring plugin's platform service when
// the type has one, else answers gRPC Unimplemented. It does not implement
// core.ExecutePlugin: the gateway sends what BuildUpstreamRequest built.
type Client struct {
	platform
	profile *upstream.Profile
	// plugin amends the built request in extend mode; nil otherwise.
	plugin core.PlatformPlugin
}

var (
	_ core.PlatformPlugin                 = (*Client)(nil)
	_ core.DeclaredUpstream               = (*Client)(nil)
	_ core.AccountModelAvailabilityPlugin = (*Client)(nil)
)

// New returns the client of the account type profile was resolved for.
// plugin is the declaring plugin's platform service, nil when it has none or
// may not see credentials: it serves the entry points the declaration does
// not cover and, when the declaration says extend, amends every built
// request (the registry only binds an extend type that has one).
func New(profile *upstream.Profile, plugin core.PlatformPlugin) *Client {
	c := &Client{platform: plugin, profile: profile}
	if plugin == nil {
		c.platform = pluginv1.UnimplementedPlatformServiceServer{}
	}
	if profile.Upstream().Extend {
		c.plugin = plugin
	}
	return c
}

// CheckModelAvailability goes to the plugin like the other entry points the
// declaration does not cover (a type declaring modelAvailability).
// Unimplemented when the plugin has no such check.
func (c *Client) CheckModelAvailability(ctx context.Context, in *pluginv1.CheckModelAvailabilityRequest) (*pluginv1.CheckModelAvailabilityResponse, error) {
	if p, ok := c.platform.(core.AccountModelAvailabilityPlugin); ok {
		return p.CheckModelAvailability(ctx, in)
	}
	return nil, status.Error(codes.Unimplemented, "the plugin has no model availability check")
}

// ValidateCredentials checks the key and base URL.
func (c *Client) ValidateCredentials(_ context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	return c.profile.Validate(in), nil
}

// BuildUpstreamRequest builds the request from the declaration; in extend
// mode the plugin then amends it (merge).
func (c *Client) BuildUpstreamRequest(ctx context.Context, in *pluginv1.BuildUpstreamRequestRequest) (*pluginv1.BuildUpstreamRequestResponse, error) {
	base, err := c.profile.Build(in)
	if err != nil || c.plugin == nil {
		return base, err
	}
	amended, err := c.plugin.BuildUpstreamRequest(ctx, &pluginv1.BuildUpstreamRequestRequest{Meta: in.GetMeta(), Account: in.GetAccount(),
		Fields: in.GetFields(), InboundHeaders: in.GetInboundHeaders(), Attempt: in.GetAttempt(), Base: base})
	if err != nil || amended == nil {
		return amended, err
	}
	return merge(base, amended, c.profile.AuthHeader()), nil
}

// merge applies the plugin's amendment to base (§4.1): its headers are
// written over base's except auth (the declaration alone decides the
// credential sent), its patches run after base's, a non-empty url or
// upstream model replaces base's.
func merge(base, amended *pluginv1.BuildUpstreamRequestResponse, auth string) *pluginv1.BuildUpstreamRequestResponse {
	out := &pluginv1.BuildUpstreamRequestResponse{Method: base.GetMethod(), Url: base.GetUrl(), UpstreamModel: base.GetUpstreamModel(),
		Headers: make(map[string]string, len(base.GetHeaders())+len(amended.GetHeaders())),
		Patches: append(append([]*pluginv1.BodyPatch(nil), base.GetPatches()...), amended.GetPatches()...)}
	for k, v := range base.GetHeaders() {
		out.Headers[strings.ToLower(k)] = v
	}
	for k, v := range amended.GetHeaders() {
		if k = strings.ToLower(k); k != auth || auth == "" {
			out.Headers[k] = v
		}
	}
	if u := amended.GetUrl(); u != "" {
		out.Url = u
	}
	if m := amended.GetUpstreamModel(); m != "" {
		out.UpstreamModel = m
	}
	return out
}

// SplitHeaders implements core.DeclaredUpstream: without extend every built
// header is the declaration's; in extend mode the declaration's are built
// again (deterministic) and a header the plugin added or changed is the
// plugin's. A plugin header equal to the declared value counts as declared.
func (c *Client) SplitHeaders(in *pluginv1.BuildUpstreamRequestRequest, built *pluginv1.BuildUpstreamRequestResponse) (declared, plugin map[string]string) {
	if c.plugin == nil {
		return built.GetHeaders(), nil
	}
	base, err := c.profile.Build(in)
	if err != nil {
		return built.GetHeaders(), nil
	}
	declared, plugin = base.GetHeaders(), map[string]string{}
	for k, v := range built.GetHeaders() {
		if bv, ok := declared[k]; !ok || bv != v {
			plugin[k] = v
		}
	}
	return declared, plugin
}

// ClassifyError classifies with the declared provider policy.
func (c *Client) ClassifyError(_ context.Context, in *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
	return c.profile.Classify(time.Now(), in), nil
}

// BuildTestRequest builds the console's account test.
func (c *Client) BuildTestRequest(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	return c.profile.Test(in)
}

// BuildModelsRequest builds the model listing request (Unimplemented when
// the declaration has none).
func (c *Client) BuildModelsRequest(_ context.Context, in *pluginv1.BuildModelsRequestRequest) (*pluginv1.BuildModelsRequestResponse, error) {
	return c.profile.Models(in)
}

// PrepareBody implements core.DeclaredUpstream.
func (c *Client) PrepareBody(body []byte, in *core.ParamOverrideContext) (core.ParamOverrideResult, error) {
	return c.profile.PrepareBody(body, in)
}

// UpstreamURL implements core.DeclaredUpstream. built's URL is the
// declaration's when rendering it for built's upstream model gives it back;
// otherwise the plugin replaced it and it is kept, as a code plugin's is.
func (c *Client) UpstreamURL(in *pluginv1.BuildUpstreamRequestRequest, built *pluginv1.BuildUpstreamRequestResponse, model string) (string, error) {
	if c.plugin != nil {
		if own, err := c.profile.URL(in, built.GetUpstreamModel()); err != nil || own != built.GetUrl() {
			return built.GetUrl(), err
		}
	}
	return c.profile.URL(in, model)
}

// AuthHeader implements core.DeclaredUpstream.
func (c *Client) AuthHeader() string { return c.profile.AuthHeader() }

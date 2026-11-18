package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/classify"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
)

const (
	// OAuth 常量（对齐 backend/internal/pkg/oauth）
	ClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AuthorizeURL = "https://claude.com/cai/oauth/authorize"
	TokenURL     = "https://platform.claude.com/v1/oauth/token"
	RedirectURI  = "https://platform.claude.com/oauth/code/callback"

	// 作用域
	ScopeOAuth     = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	ScopeInference = "user:inference"

	// 上游端点
	MessagesURL    = "https://api.anthropic.com/v1/messages"
	CountTokensURL = "https://api.anthropic.com/v1/messages/count_tokens"

	// AnthropicAPIVersion 是所有 Anthropic 面请求带的版本头（与 SDK
	// providers/anthropic 的默认版本是同一个值）；
	// OAuthBeta 是 OAuth 令牌/用量端点要求的 beta 名。
	AnthropicAPIVersion = anthropic.DefaultAPIVersion
	OAuthBeta           = "oauth-2025-04-20"

	// 控制台"测试账号"未指定模型时使用（manifest defaultModels 之一）
	testModel = "claude-sonnet-5"
)

type credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is Unix seconds; accounts imported from sub2api store it as
	// a numeric string, which json.Number accepts too.
	ExpiresAt json.Number `json:"expires_at"`
}

type Plugin struct {
	*pluginsdk.Router
	host pluginsdk.Host
	// tokenURL is where codes are exchanged: TokenURL, a fake in tests.
	tokenURL string
}

func newPlugin() *Plugin {
	p := &Plugin{Router: pluginsdk.NewRouter(), tokenURL: TokenURL}
	p.Handle("POST", "/auth/start", p.handleAuthStart)
	p.Handle("POST", "/auth/exchange", p.handleAuthExchange)
	return p
}

// Init implements pluginsdk.Initializer.
func (p *Plugin) Init(_ context.Context, h pluginsdk.Host) error {
	p.host = h
	return nil
}

// ---------------------------------------------------------------- Platform

func (p *Plugin) ValidateCredentials(ctx context.Context, req *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
	var creds credentials
	if err := json.Unmarshal([]byte(req.GetCredentialsJson()), &creds); err != nil {
		return &pluginv1.ValidateCredentialsResponse{
			Errors: []*pluginv1.FieldError{{Field: "/access_token", Code: "invalid", Message: "invalid JSON"}},
		}, nil
	}
	if strings.TrimSpace(creds.AccessToken) == "" {
		return &pluginv1.ValidateCredentialsResponse{
			Errors: []*pluginv1.FieldError{{Field: "/access_token", Code: "required", Message: "access_token is required"}},
		}, nil
	}
	return &pluginv1.ValidateCredentialsResponse{}, nil
}

// upstreamURLs maps the protocols the manifest routes here to their
// upstream URLs. Anything else is refused: a protocol without its own URL
// must never fall back to the Messages URL, where it would run a generation.
var upstreamURLs = map[string]string{
	anthropic.ProtocolMessages:    MessagesURL,
	anthropic.ProtocolCountTokens: CountTokensURL,
}

// BuildUpstream builds the final request of one attempt: Messages or
// count_tokens, by the protocol. The body goes out as the host handed it
// over. Every header is authoritative, written over the operator's
// passthrough and overrides, as a code plugin's headers always were: the
// bearer token and the oauth beta are what an OAuth token is accepted with
// on both endpoints, and the version and content type stay as the request
// was built.
func (p *Plugin) BuildUpstream(_ context.Context, in *pluginv1.ExecuteRequest) (*pluginv1.UpstreamRequest, error) {
	if in.GetMeta() == nil || in.GetAccount() == nil {
		return nil, status.Error(codes.InvalidArgument, "missing meta or account")
	}
	target, ok := upstreamURLs[in.GetMeta().GetProtocol()]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported protocol %q", in.GetMeta().GetProtocol())
	}
	// The host renews the access token before it expires (refresh.go).
	var creds credentials
	if err := json.Unmarshal([]byte(in.GetAccount().GetCredentialsJson()), &creds); err != nil {
		return nil, status.Error(codes.InvalidArgument, "parse credentials: "+err.Error())
	}
	return pluginsdk.NewUpstream(http.MethodPost, target).
		HeaderMap(oauthHeaders(creds.AccessToken)).
		Body(in.GetBody()).
		Build()
}

// oauthHeaders are the headers of every request made with an OAuth access
// token: a bearer token plus the oauth beta, never an x-api-key. Only the
// oauth beta, as sub2api's gateway_claude_oauth_body.go adds it; count_tokens
// is GA and needs no beta of its own.
func oauthHeaders(accessToken string) map[string]string {
	return map[string]string{
		"authorization":     "Bearer " + accessToken,
		"anthropic-version": AnthropicAPIVersion,
		"anthropic-beta":    OAuthBeta,
		"content-type":      "application/json",
	}
}

// classifyPolicy is Anthropic's policy (error.type and error.message,
// credentials, credit balance, rate limits, 529, 5xx; anything else goes back
// to the client) plus the one OAuth-specific reading: a 401 means the access
// token expired or was revoked, which re-authorizing fixes.
var classifyPolicy = anthropic.Policy.With(classify.Rule{
	When: classify.Status(http.StatusUnauthorized), Effect: classify.Disable,
	Reason: "OAuth token expired or invalid (%d)",
})

func (p *Plugin) ClassifyError(_ context.Context, req *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
	return classifyPolicy.Classify(time.Now(), req), nil
}

func (p *Plugin) BuildTestRequest(ctx context.Context, req *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
	acc := req.GetAccount()
	var creds credentials
	if err := json.Unmarshal([]byte(acc.GetCredentialsJson()), &creds); err != nil {
		return nil, fmt.Errorf("invalid credentials JSON: %w", err)
	}
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("missing access_token")
	}

	model := req.GetModel()
	if model == "" {
		model = testModel
	}
	testBody := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"messages": []map[string]string{
			{"role": "user", "content": "Hi"},
		},
	}
	bodyBytes, _ := json.Marshal(testBody)

	return &pluginv1.BuildTestRequestResponse{
		Method:        "POST",
		Url:           MessagesURL,
		Headers:       oauthHeaders(creds.AccessToken),
		BodyJson:      string(bodyBytes),
		Model:         model,
		UsageProtocol: anthropic.ProtocolMessages,
	}, nil
}

// ---------------------------------------------------------------- HTTP (OAuth 流程)

type authStartRequest struct {
	Scope   string `json:"scope"`    // "oauth" 或 "inference"
	ProxyID *int64 `json:"proxy_id"` // 可选
}

// kvOAuthSession is the KV namespace of authorization sessions.
const kvOAuthSession = "oauth_session"

// authSession is the stored state of one authorization (a caller session).
type authSession struct {
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	Scope        string `json:"scope"`
	CreatedAt    int64  `json:"created_at"`
}

type authStartResponse struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}

func (p *Plugin) handleAuthStart(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var input authStartRequest
	if err := pluginsdk.DecodeJSON(req, &input); err != nil {
		return pluginsdk.ErrorResponse(400, "invalid_request", "invalid JSON"), nil
	}

	scope := ScopeOAuth
	if input.Scope == "inference" {
		scope = ScopeInference
	}

	// 生成 PKCE
	state, err := generateRandomString(32)
	if err != nil {
		return pluginsdk.ErrorResponse(500, "internal", "failed to generate state"), nil
	}
	verifier, err := generateRandomString(32)
	if err != nil {
		return pluginsdk.ErrorResponse(500, "internal", "failed to generate verifier"), nil
	}
	challenge := generateCodeChallenge(verifier)

	// 存储会话（KV，绑定发起人：只有同一用户能用 session_id 换取凭证）
	session := authSession{State: state, CodeVerifier: verifier, Scope: scope, CreatedAt: time.Now().Unix()}
	sessionID, err := pluginsdk.SaveCallerSession(ctx, p.host.KV(), kvOAuthSession, req, session, 30*time.Minute)
	if errors.Is(err, pluginsdk.ErrNoCaller) {
		return pluginsdk.Unauthenticated(), nil
	}
	if err != nil {
		return pluginsdk.ErrorResponse(500, "internal", "failed to store session"), nil
	}

	// 构造授权 URL
	authURL := buildAuthorizationURL(state, challenge, scope)

	return pluginsdk.JSONResponse(200, authStartResponse{
		AuthURL:   authURL,
		SessionID: sessionID,
	}), nil
}

type authExchangeRequest struct {
	SessionID string `json:"session_id"`
	Code      string `json:"code"`
}

func (p *Plugin) handleAuthExchange(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var input authExchangeRequest
	if err := pluginsdk.DecodeJSON(req, &input); err != nil {
		return pluginsdk.ErrorResponse(400, "invalid_request", "invalid JSON"), nil
	}

	// 获取会话：不存在、已过期与他人的会话不可区分
	var session authSession
	ok, err := pluginsdk.LoadCallerSession(ctx, p.host.KV(), kvOAuthSession, input.SessionID, req, &session)
	if err != nil {
		return pluginsdk.ErrorResponse(500, "internal", "failed to retrieve session"), nil
	}
	if !ok {
		return pluginsdk.ErrorResponse(404, "not_found", "session not found or expired"), nil
	}

	// 解析 code（可能包含 state）；粘贴时带上的首尾空白不算内容
	code, codeState, _ := strings.Cut(strings.TrimSpace(input.Code), "#")
	code, codeState = strings.TrimSpace(code), strings.TrimSpace(codeState)

	// code 带回的 state 必须是本会话发出的；不一致说明 code 属于另一次授权，
	// 会话作废，需重新发起。这只防止粘错：不带 state 的 code 仍会转给上游，
	// 由 PKCE verifier 保证它属于本会话。
	if codeState != "" && subtle.ConstantTimeCompare([]byte(codeState), []byte(session.State)) != 1 {
		_ = p.host.KV().Delete(ctx, kvOAuthSession, input.SessionID)
		return pluginsdk.ErrorResponse(400, "state_mismatch", "the code does not belong to this authorization session; start again"), nil
	}

	// 交换 token
	tokenResp, err := p.exchangeCode(ctx, code, session.CodeVerifier, codeState)
	if err != nil {
		return pluginsdk.ErrorResponse(500, "exchange_failed", err.Error()), nil
	}

	// 删除会话
	_ = p.host.KV().Delete(ctx, kvOAuthSession, input.SessionID)

	// 返回凭证（供控制台填充账号表单）
	return pluginsdk.JSONResponse(200, map[string]any{
		"access_token":  tokenResp.AccessToken,
		"refresh_token": tokenResp.RefreshToken,
		"expires_at":    time.Now().Unix() + tokenResp.ExpiresIn,
		"org_uuid":      tokenResp.Organization.UUID,
		"account_uuid":  tokenResp.Account.UUID,
		"email_address": tokenResp.Account.EmailAddress,
	}), nil
}

// ---------------------------------------------------------------- helpers

func generateRandomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64URLEncode(b), nil
}

func base64URLEncode(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func generateCodeChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return base64URLEncode(hash[:])
}

func buildAuthorizationURL(state, codeChallenge, scope string) string {
	encodedRedirectURI := url.QueryEscape(RedirectURI)
	encodedScope := strings.ReplaceAll(url.QueryEscape(scope), "%20", "+")
	return fmt.Sprintf("%s?code=true&client_id=%s&response_type=code&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
		AuthorizeURL, ClientID, encodedRedirectURI, encodedScope, codeChallenge, state)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization,omitempty"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account,omitempty"`
}

func (p *Plugin) exchangeCode(ctx context.Context, code, verifier, state string) (*tokenResponse, error) {
	body := map[string]any{
		"code":          code,
		"grant_type":    "authorization_code",
		"client_id":     ClientID,
		"redirect_uri":  RedirectURI,
		"code_verifier": verifier,
	}
	if state != "" {
		body["state"] = state
	}

	bodyJSON, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", p.tokenURL, strings.NewReader(string(bodyJSON)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("token exchange failed: %d %s", resp.StatusCode, string(respBody))
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}

	return &tokenResp, nil
}

// ---------------------------------------------------------------- main

func main() {
	pluginsdk.Serve(newPlugin(), pluginsdk.WithManifest(manifestJSON))
}

//go:embed manifest.json
var manifestJSON []byte

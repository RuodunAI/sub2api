package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

//go:embed manifest.json
var manifestRaw []byte

func TestManifest(t *testing.T) {
	var m manifest.Manifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if m.Key != "claude_oauth" {
		t.Errorf("key = %q, want claude_oauth", m.Key)
	}
	if len(m.AccountTypes) != 2 {
		t.Fatalf("account types = %d, want 2", len(m.AccountTypes))
	}
	oauth := m.AccountTypes[0]
	if oauth.ID != "claude_oauth" {
		t.Errorf("accountTypes[0].id = %q, want claude_oauth", oauth.ID)
	}
	if len(oauth.Platforms) != 1 || oauth.Platforms[0].Platform != "anthropic" {
		t.Errorf("accountTypes[0] platform = %+v, want anthropic", oauth.Platforms)
	}
	if len(oauth.SensitiveFields) != 2 {
		t.Errorf("sensitiveFields = %v, want 2 fields", oauth.SensitiveFields)
	}
	setupToken := m.AccountTypes[1]
	if setupToken.ID != "claude_setup_token" {
		t.Errorf("accountTypes[1].id = %q, want claude_setup_token", setupToken.ID)
	}
}

func TestValidateCredentials(t *testing.T) {
	h := pluginsdktest.Start(t, newPlugin(), pluginsdktest.Options{
		SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestRaw)},
	})

	tests := []struct {
		name      string
		creds     string
		wantError bool
	}{
		{
			name:      "valid",
			creds:     `{"access_token":"sk-ant-test"}`,
			wantError: false,
		},
		{
			name:      "missing access_token",
			creds:     `{}`,
			wantError: true,
		},
		{
			name:      "empty access_token",
			creds:     `{"access_token":""}`,
			wantError: true,
		},
		{
			name:      "invalid JSON",
			creds:     `{invalid`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := h.Platform.ValidateCredentials(context.Background(), &pluginv1.ValidateCredentialsRequest{
				AccountType:     "claude_oauth",
				CredentialsJson: tt.creds,
			})
			if err != nil {
				t.Fatalf("ValidateCredentials error: %v", err)
			}
			hasError := len(resp.GetErrors()) > 0
			if hasError != tt.wantError {
				t.Errorf("errors = %v, wantError = %v", resp.GetErrors(), tt.wantError)
			}
		})
	}
}

// TestClassifyError feeds real-shaped Anthropic error bodies: the type is at
// error.type, the top-level type is always "error".
func TestClassifyError(t *testing.T) {
	h := pluginsdktest.Start(t, newPlugin(), pluginsdktest.Options{
		SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestRaw)},
	})
	body := func(typ, msg string) string {
		return `{"type":"error","error":{"type":"` + typ + `","message":"` + msg + `"},"request_id":"req_011"}`
	}
	const (
		returned = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
	)
	tests := []struct {
		name       string
		status     int32
		headers    map[string]string
		body       string
		wantAction pluginv1.ClassifyErrorResponse_Action
		wantEffect pluginv1.ClassifyErrorResponse_AccountEffect
		wantType   string
		wantReason string
	}{
		{"400 prompt too long", 400, nil, body("invalid_request_error", "prompt is too long: 1000001 tokens > 1000000 maximum"),
			returned, none, "invalid_request_error", ""},
		{"400 credit balance", 400, nil, body("invalid_request_error", "Your credit balance is too low to access the Anthropic API."),
			failover, disable, "invalid_request_error", "upstream credit balance too low"},
		{"401 token expired", 401, nil, body("authentication_error", "OAuth token has expired."),
			failover, disable, "authentication_error", "OAuth token expired or invalid (401): OAuth token has expired."},
		{"403 permission", 403, nil, body("permission_error", "OAuth token does not meet scope requirement user:inference"),
			failover, disable, "permission_error", "upstream rejected credentials (403)"},
		{"404 model", 404, nil, body("not_found_error", "model: claude-nope"),
			returned, none, "not_found_error", ""},
		{"429 rate limit", 429, map[string]string{"retry-after": "30"}, body("rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit"),
			failover, cooldown, "rate_limit_error", "rate limited"},
		{"529 overloaded", 529, nil, body("overloaded_error", "Overloaded"),
			failover, cooldown, "overloaded_error", "upstream overloaded (529)"},
		{"500 api error", 500, nil, body("api_error", "Internal server error"),
			failover, cooldown, "api_error", "upstream server error (500)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := h.Platform.ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
				Status: tt.status, Headers: tt.headers, BodyPrefix: []byte(tt.body),
			})
			if err != nil {
				t.Fatalf("ClassifyError error: %v", err)
			}
			if resp.GetAction() != tt.wantAction || resp.GetAccountEffect() != tt.wantEffect || resp.GetClientErrorType() != tt.wantType {
				t.Errorf("action %v effect %v type %q, want %v %v %q", resp.GetAction(), resp.GetAccountEffect(), resp.GetClientErrorType(), tt.wantAction, tt.wantEffect, tt.wantType)
			}
			if !strings.HasPrefix(resp.GetReason(), tt.wantReason) {
				t.Errorf("reason %q, want prefix %q", resp.GetReason(), tt.wantReason)
			}
			if tt.wantAction == returned && resp.GetClientMessage() == "" {
				t.Errorf("the upstream message is not passed to the client")
			}
		})
	}
}

func TestAuthStartFlow(t *testing.T) {
	h := pluginsdktest.Start(t, newPlugin(), pluginsdktest.Options{
		SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestRaw)},
		Grants: []*pluginv1.Grant{
			{Permission: "kv"},
			{Permission: "routes.admin"},
		},
	})

	// 启动授权流程
	resp := h.Do("POST", "/auth/start", nil, map[string]any{"scope": "oauth"})
	if resp.GetStatus() != 200 {
		t.Fatalf("auth/start status = %d, want 200, body=%s", resp.GetStatus(), resp.GetBody())
	}

	var startResp authStartResponse
	if err := json.Unmarshal(resp.GetBody(), &startResp); err != nil {
		t.Fatalf("parse start response: %v", err)
	}

	if startResp.AuthURL == "" {
		t.Error("auth_url is empty")
	}
	if !contains(startResp.AuthURL, "claude.com") {
		t.Errorf("auth_url does not contain claude.com: %s", startResp.AuthURL)
	}
	if startResp.SessionID == "" {
		t.Error("session_id is empty")
	}

	// 验证会话已存储
	kvResp, err := h.Host.KVGet(context.Background(), &pluginv1.KVGetRequest{
		Namespace: "oauth_session",
		Key:       startResp.SessionID,
	})
	if err != nil {
		t.Fatalf("KVGet error: %v", err)
	}
	if !kvResp.GetFound() {
		t.Fatal("session not found in KV")
	}
	// 会话绑定发起人（Harness.Do 的调用者是用户 1）
	var stored struct {
		UserID  int64       `json:"uid"`
		Session authSession `json:"v"`
	}
	if err := json.Unmarshal(kvResp.GetValue(), &stored); err != nil {
		t.Fatalf("parse session: %v", err)
	}
	if stored.UserID != 1 || stored.Session.Scope != ScopeOAuth || stored.Session.State == "" || stored.Session.CodeVerifier == "" {
		t.Errorf("stored session = %+v", stored)
	}
}

// startAuth starts an authorization as user 1 and returns its session id and
// state.
func startAuth(t *testing.T, h *pluginsdktest.Harness) (id, state string) {
	t.Helper()
	resp := h.Do("POST", "/auth/start", nil, map[string]any{"scope": "oauth"})
	var start authStartResponse
	if err := json.Unmarshal(resp.GetBody(), &start); err != nil || resp.GetStatus() != 200 {
		t.Fatalf("auth/start = %d %s (%v)", resp.GetStatus(), resp.GetBody(), err)
	}
	u, err := url.Parse(start.AuthURL)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("auth url %q: %v", start.AuthURL, err)
	}
	return start.SessionID, u.Query().Get("state")
}

func sessionStored(t *testing.T, h *pluginsdktest.Harness, id string) bool {
	t.Helper()
	kv, err := h.Host.KVGet(context.Background(), &pluginv1.KVGetRequest{Namespace: kvOAuthSession, Key: id})
	if err != nil {
		t.Fatal(err)
	}
	return kv.GetFound()
}

func errorCode(t *testing.T, resp *pluginv1.HTTPResponse) string {
	t.Helper()
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.GetBody(), &out); err != nil {
		t.Fatalf("parse error body %s: %v", resp.GetBody(), err)
	}
	return out.Error.Code
}

func authHarness(t *testing.T) *pluginsdktest.Harness {
	t.Helper()
	h, _ := authHarnessWithIssuer(t)
	return h
}

// authHarnessWithIssuer is authHarness with codes exchanged at a fake token
// endpoint, never the real one; the channel receives each exchange body.
func authHarnessWithIssuer(t *testing.T) (*pluginsdktest.Harness, <-chan map[string]any) {
	t.Helper()
	exchanged := make(chan map[string]any, 4)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		exchanged <- body
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
	}))
	t.Cleanup(issuer.Close)
	p := newPlugin()
	p.tokenURL = issuer.URL
	return pluginsdktest.Start(t, p, pluginsdktest.Options{
		SDK:    []pluginsdk.Option{pluginsdk.WithManifest(manifestRaw)},
		Grants: []*pluginv1.Grant{{Permission: "kv"}, {Permission: "routes.admin"}},
	}), exchanged
}

// Only the user who started an authorization can exchange its code: to anyone
// else the session does not exist, and it is left for its owner.
func TestAuthExchangeNeedsTheStartingUser(t *testing.T) {
	h := authHarness(t)
	id, state := startAuth(t, h)
	body := map[string]any{"session_id": id, "code": "code-1#" + state}
	missing := h.Do("POST", "/auth/exchange", nil, map[string]any{"session_id": "no-such-session", "code": "code-1"})
	for _, uid := range []int64{2, 0} {
		resp := h.DoAs(uid, "POST", "/auth/exchange", nil, body)
		if resp.GetStatus() != 404 || string(resp.GetBody()) != string(missing.GetBody()) {
			t.Fatalf("user %d = %d %s, want the answer of a missing session %s", uid, resp.GetStatus(), resp.GetBody(), missing.GetBody())
		}
	}
	if !sessionStored(t, h, id) {
		t.Fatal("another user's attempt removed the session")
	}
	if resp := h.DoAs(0, "POST", "/auth/start", nil, map[string]any{"scope": "oauth"}); resp.GetStatus() != 401 {
		t.Fatalf("auth/start without a caller = %d %s", resp.GetStatus(), resp.GetBody())
	}
}

// Whitespace pasted around the code or its state is not a different state.
func TestAuthExchangeStateIgnoresPastedWhitespace(t *testing.T) {
	h, exchanged := authHarnessWithIssuer(t)
	id, state := startAuth(t, h)
	resp := h.Do("POST", "/auth/exchange", nil, map[string]any{"session_id": id, "code": "  code-1#" + state + " \n"})
	if resp.GetStatus() != 200 {
		t.Fatalf("padded state = %d %s", resp.GetStatus(), resp.GetBody())
	}
	if body := <-exchanged; body["code"] != "code-1" || body["state"] != state {
		t.Fatalf("exchanged %v, want code-1 and state %q", body, state)
	}
	if sessionStored(t, h, id) {
		t.Fatal("the session survived a completed exchange")
	}
}

// A code carrying another authorization's state is refused before anything
// is sent upstream, and the session is over.
func TestAuthExchangeStateMismatch(t *testing.T) {
	h := authHarness(t)
	id, state := startAuth(t, h)
	resp := h.Do("POST", "/auth/exchange", nil, map[string]any{"session_id": id, "code": "code-1#not-" + state})
	if resp.GetStatus() != 400 || errorCode(t, resp) != "state_mismatch" {
		t.Fatalf("mismatched state = %d %s", resp.GetStatus(), resp.GetBody())
	}
	if sessionStored(t, h, id) {
		t.Fatal("the session survived a state mismatch")
	}
	if resp := h.Do("POST", "/auth/exchange", nil, map[string]any{"session_id": id, "code": "code-1#" + state}); resp.GetStatus() != 404 {
		t.Fatalf("exchange after a mismatch = %d %s", resp.GetStatus(), resp.GetBody())
	}
}

func TestPKCE(t *testing.T) {
	verifier, err := generateRandomString(32)
	if err != nil {
		t.Fatalf("generateRandomString: %v", err)
	}
	if len(verifier) < 40 {
		t.Errorf("verifier length = %d, too short", len(verifier))
	}

	challenge := generateCodeChallenge(verifier)
	if len(challenge) < 40 {
		t.Errorf("challenge length = %d, too short", len(challenge))
	}
	if challenge == verifier {
		t.Error("challenge should differ from verifier")
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

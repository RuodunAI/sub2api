package vertex

import (
	"context"
	"net/http"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

func googleError(code int, status, message string) []byte {
	return []byte(mustJSON(map[string]any{"error": map[string]any{"code": code, "status": status, "message": message}}))
}

// TestClassifyError is the table of DESIGN-2026-11-17-CLOUD-PROVIDERS §7.4.
func TestClassifyError(t *testing.T) {
	const (
		ret      = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	keyed := []string{TypeServiceAccount, TypeAuthorizedUser}
	all := []string{TypeServiceAccount, TypeAuthorizedUser, TypeAPIKey}
	retryInfo := []byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17s"}]}}`)

	for _, tc := range []struct {
		name       string
		types      []string
		status     int32
		headers    map[string]string
		body       []byte
		transport  string
		wantAction pluginv1.ClassifyErrorResponse_Action
		wantEffect pluginv1.ClassifyErrorResponse_AccountEffect
		// wantCooldown is the cooldown in seconds from testNow (0: none).
		wantCooldown int64
		wantType     string
	}{
		{"401 on a token account", keyed, 401, nil, googleError(401, "UNAUTHENTICATED", "Request had invalid authentication credentials."),
			"", failover, cooldown, 60, "UNAUTHENTICATED"},
		// A type the plugin does not know is never disabled by a 401.
		{"401 on an unknown or missing type", []string{"", "vertex_other"}, 401, nil, googleError(401, "UNAUTHENTICATED", "Request had invalid authentication credentials."),
			"", failover, cooldown, 60, "UNAUTHENTICATED"},
		{"401 on an API key", []string{TypeAPIKey}, 401, nil, googleError(401, "UNAUTHENTICATED", "API keys are not supported by this API."),
			"", failover, disable, 0, "UNAUTHENTICATED"},
		{"403", all, 403, nil, googleError(403, "PERMISSION_DENIED", "Permission 'aiplatform.endpoints.predict' denied"),
			"", failover, disable, 0, "PERMISSION_DENIED"},
		{"400 invalid argument", all, 400, nil, googleError(400, "INVALID_ARGUMENT", "Please use a valid role: user, model."),
			"", ret, none, 0, "INVALID_ARGUMENT"},
		{"400 API key not valid", all, 400, nil, googleError(400, "INVALID_ARGUMENT", "API key not valid. Please pass a valid API key."),
			"", failover, disable, 0, "INVALID_ARGUMENT"},
		{"404", all, 404, nil, googleError(404, "NOT_FOUND", "Publisher Model was not found"), "", ret, none, 0, "NOT_FOUND"},
		{"429 with retry-after", all, 429, map[string]string{"Retry-After": "42"}, googleError(429, "RESOURCE_EXHAUSTED", "Quota exceeded"),
			"", failover, cooldown, 42, "RESOURCE_EXHAUSTED"},
		{"429 with RetryInfo", all, 429, nil, retryInfo, "", failover, cooldown, 17, "RESOURCE_EXHAUSTED"},
		{"429 without a hint", all, 429, nil, googleError(429, "RESOURCE_EXHAUSTED", "Resource exhausted"), "", failover, cooldown, 60, "RESOURCE_EXHAUSTED"},
		{"500", all, 500, nil, googleError(500, "INTERNAL", "Internal error"), "", failover, none, 0, "INTERNAL"},
		{"503", all, 503, nil, googleError(503, "UNAVAILABLE", "The model is overloaded"), "", failover, none, 0, "UNAVAILABLE"},
		{"transport error", all, 0, nil, nil, "connection reset", failover, cooldown, 10, "UNAVAILABLE"},
	} {
		for _, typ := range tc.types {
			t.Run(tc.name+"/"+typ, func(t *testing.T) {
				resp, err := newTestPlugin(Endpoints{}).ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
					Meta:    &pluginv1.RequestMeta{Protocol: ProtocolGenerate},
					Account: &pluginv1.Account{Type: typ},
					Status:  tc.status, Headers: tc.headers, BodyPrefix: tc.body, TransportError: tc.transport,
				})
				if err != nil {
					t.Fatal(err)
				}
				if resp.GetAction() != tc.wantAction || resp.GetAccountEffect() != tc.wantEffect || resp.GetClientErrorType() != tc.wantType {
					t.Fatalf("action %s effect %s type %s (%s)", resp.GetAction(), resp.GetAccountEffect(), resp.GetClientErrorType(), resp.GetReason())
				}
				var wantUntil int64
				if tc.wantCooldown != 0 {
					wantUntil = testNow.Unix() + tc.wantCooldown
				}
				if resp.GetCooldownUntilUnix() != wantUntil {
					t.Fatalf("cooldown until %d, want %d", resp.GetCooldownUntilUnix(), wantUntil)
				}
			})
		}
	}
}

// The plugin's rule must not change the shared Gemini policy.
func TestTokenPolicyLeavesGeminiPolicyAlone(t *testing.T) {
	resp, err := newTestPlugin(Endpoints{}).ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
		Account: &pluginv1.Account{Type: TypeServiceAccount}, Status: http.StatusUnauthorized,
	})
	if err != nil || resp.GetAccountEffect() != pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN {
		t.Fatalf("%v %v", resp, err)
	}
	resp, err = newTestPlugin(Endpoints{}).ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
		Account: &pluginv1.Account{Type: TypeAPIKey}, Status: http.StatusUnauthorized,
	})
	if err != nil || resp.GetAccountEffect() != pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE {
		t.Fatalf("%v %v", resp, err)
	}
}

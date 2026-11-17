package vertex

import (
	"context"
	"maps"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
)

func buildRequest(acc *pluginv1.Account, protocol, model string) *pluginv1.BuildUpstreamRequestRequest {
	return &pluginv1.BuildUpstreamRequestRequest{
		Account: acc,
		Meta:    &pluginv1.RequestMeta{Protocol: protocol, Model: model, Stream: protocol == ProtocolStreamGenerate},
	}
}

func TestUpstreamURLs(t *testing.T) {
	apiKey := map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}
	const (
		regional = "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project-123456/locations/us-central1/publishers/google/models/"
		global   = "https://aiplatform.googleapis.com/v1/projects/my-project-123456/locations/global/publishers/google/models/"
		express  = "https://aiplatform.googleapis.com/v1/publishers/google/models/"
	)
	globalTarget := map[string]any{fieldProject: testProject, fieldLocation: "global"}

	for _, tc := range []struct {
		name     string
		acc      *pluginv1.Account
		protocol string
		model    string
		wantURL  string
		wantID   string
	}{
		{"regional generate", saAccount(fullTarget), ProtocolGenerate, "gemini-2.5-flash", regional + "gemini-2.5-flash:generateContent", "gemini-2.5-flash"},
		{"regional stream", saAccount(fullTarget), ProtocolStreamGenerate, "gemini-2.5-flash", regional + "gemini-2.5-flash:streamGenerateContent?alt=sse", "gemini-2.5-flash"},
		{"regional countTokens", saAccount(fullTarget), ProtocolCountTokens, "gemini-2.5-flash", regional + "gemini-2.5-flash:countTokens", "gemini-2.5-flash"},
		{"global generate", saAccount(globalTarget), ProtocolGenerate, "gemini-2.5-pro", global + "gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{"global stream", saAccount(globalTarget), ProtocolStreamGenerate, "gemini-2.5-pro", global + "gemini-2.5-pro:streamGenerateContent?alt=sse", "gemini-2.5-pro"},
		{"another region", saAccount(map[string]any{fieldProject: testProject, fieldLocation: "europe-west4"}), ProtocolGenerate, "gemini-2.5-pro",
			"https://europe-west4-aiplatform.googleapis.com/v1/projects/my-project-123456/locations/europe-west4/publishers/google/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro"},
		{"models/ prefix trimmed", saAccount(fullTarget), ProtocolGenerate, "models/gemini-2.5-flash", regional + "gemini-2.5-flash:generateContent", "gemini-2.5-flash"},
		{"model escaped as one segment", saAccount(fullTarget), ProtocolGenerate, "tuned/model?x#y", regional + "tuned%2Fmodel%3Fx%23y:generateContent", "tuned/model?x#y"},
		{"user credentials", stored(TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(nil), fieldAccessToken: "ya29.token"}, fullTarget),
			ProtocolGenerate, "gemini-2.5-flash", regional + "gemini-2.5-flash:generateContent", "gemini-2.5-flash"},
		{"api key express generate", stored(TypeAPIKey, apiKey, nil), ProtocolGenerate, "gemini-2.5-flash", express + "gemini-2.5-flash:generateContent", "gemini-2.5-flash"},
		{"api key express stream", stored(TypeAPIKey, apiKey, map[string]any{fieldProject: "", fieldLocation: ""}), ProtocolStreamGenerate, "models/gemini-2.5-flash",
			express + "gemini-2.5-flash:streamGenerateContent?alt=sse", "gemini-2.5-flash"},
		{"api key express countTokens", stored(TypeAPIKey, apiKey, nil), ProtocolCountTokens, "gemini-2.5-flash", express + "gemini-2.5-flash:countTokens", "gemini-2.5-flash"},
		{"api key with project", stored(TypeAPIKey, apiKey, fullTarget), ProtocolGenerate, "gemini-2.5-flash", regional + "gemini-2.5-flash:generateContent", "gemini-2.5-flash"},
		{"api key with project, global", stored(TypeAPIKey, apiKey, globalTarget), ProtocolStreamGenerate, "gemini-2.5-flash",
			global + "gemini-2.5-flash:streamGenerateContent?alt=sse", "gemini-2.5-flash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := newTestPlugin(Endpoints{}).BuildUpstreamRequest(context.Background(), buildRequest(tc.acc, tc.protocol, tc.model))
			if err != nil {
				t.Fatalf("BuildUpstreamRequest: %v", err)
			}
			if resp.GetMethod() != http.MethodPost || resp.GetUrl() != tc.wantURL || resp.GetUpstreamModel() != tc.wantID {
				t.Fatalf("request = %s %s (model %s)\nwant POST %s (model %s)", resp.GetMethod(), resp.GetUrl(), resp.GetUpstreamModel(), tc.wantURL, tc.wantID)
			}
			// The Gemini body is forwarded as it is.
			if len(resp.GetPatches()) != 0 {
				t.Fatalf("patches = %v", resp.GetPatches())
			}
		})
	}
}

func TestUpstreamHeaders(t *testing.T) {
	p := newTestPlugin(Endpoints{})
	for _, tc := range []struct {
		name string
		acc  *pluginv1.Account
		want map[string]string
	}{
		{"service account", saAccount(fullTarget), map[string]string{"content-type": "application/json", "authorization": "Bearer ya29.token"}},
		// A token that has not expired yet is sent.
		{"service account, token still valid", stored(TypeServiceAccount, map[string]any{
			"service_account_json": saJSON(nil), fieldAccessToken: "ya29.token", fieldExpiresAt: testNow.Unix() + 1}, fullTarget),
			map[string]string{"content-type": "application/json", "authorization": "Bearer ya29.token"}},
		// User credentials belong to no project: the account's project is
		// named as the quota project, or aiplatform answers 403.
		{"user credentials", stored(TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(nil), fieldAccessToken: " ya29.user "}, fullTarget),
			map[string]string{"content-type": "application/json", "authorization": "Bearer ya29.user", "x-goog-user-project": testProject}},
		{"user credentials, the account's own project", stored(TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(nil), fieldAccessToken: "ya29.user"},
			map[string]any{fieldProject: "another-project-9", fieldLocation: "global"}),
			map[string]string{"content-type": "application/json", "authorization": "Bearer ya29.user", "x-goog-user-project": "another-project-9"}},
		{"api key", stored(TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, nil),
			map[string]string{"content-type": "application/json", "x-goog-api-key": "AQ.Ab8RN6-example"}},
		// A token left in an API key account is not sent.
		{"api key with a stray token", stored(TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example", fieldAccessToken: "ya29.stray"}, fullTarget),
			map[string]string{"content-type": "application/json", "x-goog-api-key": "AQ.Ab8RN6-example"}},
	} {
		resp, err := p.BuildUpstreamRequest(context.Background(), buildRequest(tc.acc, ProtocolGenerate, "gemini-2.5-flash"))
		if err != nil || !maps.Equal(resp.GetHeaders(), tc.want) {
			t.Errorf("%s: headers = %v, err %v", tc.name, resp.GetHeaders(), err)
		}
	}
}

func TestUpstreamRefusals(t *testing.T) {
	p := newTestPlugin(Endpoints{})
	newSA := stored(TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil), fieldExpiresAt: expiredAt}, fullTarget)
	for _, tc := range []struct {
		name     string
		acc      *pluginv1.Account
		protocol string
		model    string
		want     codes.Code
	}{
		// Waiting for the first token: the gateway moves on to another
		// account and does not penalize this one.
		{"no token yet", newSA, ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"user without token", stored(TypeAuthorizedUser, map[string]any{"authorized_user_json": userJSON(nil)}, fullTarget), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		// An expired token is not sent either: the account waits for the
		// refresh like one that has no token yet.
		{"token expired", stored(TypeServiceAccount, map[string]any{
			"service_account_json": saJSON(nil), fieldAccessToken: "ya29.old", fieldExpiresAt: testNow.Unix() - 1}, fullTarget), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"token expiring now", stored(TypeAuthorizedUser, map[string]any{
			"authorized_user_json": userJSON(nil), fieldAccessToken: "ya29.old", fieldExpiresAt: testNow.Unix()}, fullTarget), ProtocolStreamGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"token expired, expiry as a string", stored(TypeServiceAccount, map[string]any{
			"service_account_json": saJSON(nil), fieldAccessToken: "ya29.old", fieldExpiresAt: "1700000000"}, fullTarget), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"api key missing", stored(TypeAPIKey, map[string]any{}, nil), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"service account without project", saAccount(nil), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"service account without location", saAccount(map[string]any{fieldProject: testProject}), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"api key with project only", stored(TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, map[string]any{fieldProject: testProject}), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		// The location becomes part of the host: nothing but a location passes.
		{"location is a host", saAccount(map[string]any{fieldProject: testProject, fieldLocation: "evil.example/x"}), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"location with userinfo", saAccount(map[string]any{fieldProject: testProject, fieldLocation: "a@evil.example"}), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"project with a path", saAccount(map[string]any{fieldProject: "p/../../x", fieldLocation: "global"}), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"unknown type", stored("vertex_other", map[string]any{}, fullTarget), ProtocolGenerate, "gemini-2.5-flash", codes.FailedPrecondition},
		{"unknown protocol", saAccount(fullTarget), "openai.chat", "gemini-2.5-flash", codes.InvalidArgument},
		{"no model", saAccount(fullTarget), ProtocolGenerate, "", codes.InvalidArgument},
		{"only the prefix", saAccount(fullTarget), ProtocolGenerate, "models/", codes.InvalidArgument},
		{"credentials not JSON", &pluginv1.Account{Type: TypeServiceAccount, CredentialsJson: "{", SettingsJson: mustJSON(fullTarget)}, ProtocolGenerate, "gemini-2.5-flash", codes.InvalidArgument},
	} {
		resp, err := p.BuildUpstreamRequest(context.Background(), buildRequest(tc.acc, tc.protocol, tc.model))
		if status.Code(err) != tc.want || resp != nil {
			t.Errorf("%s: %v %v, want %s", tc.name, resp, err, tc.want)
		}
	}
}

func TestBuildTestRequest(t *testing.T) {
	p := newTestPlugin(Endpoints{})
	resp, err := p.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{Account: saAccount(fullTarget)})
	if err != nil {
		t.Fatal(err)
	}
	wantURL := "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project-123456/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"
	if resp.GetMethod() != http.MethodPost || resp.GetUrl() != wantURL || resp.GetModel() != DefaultTestModel ||
		resp.GetUsageProtocol() != ProtocolGenerate || resp.GetBodyJson() != gemini.GenerateContentPingBody(1) ||
		resp.GetHeaders()["authorization"] != "Bearer ya29.token" {
		t.Fatalf("test request = %+v", resp)
	}

	// The model the operator names, on an express API key account.
	resp, err = p.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{
		Account: stored(TypeAPIKey, map[string]any{fieldAPIKey: "AQ.Ab8RN6-example"}, nil), Model: " models/gemini-2.5-pro ",
	})
	if err != nil || resp.GetUrl() != "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-2.5-pro:generateContent" ||
		resp.GetModel() != "gemini-2.5-pro" || resp.GetHeaders()["x-goog-api-key"] != "AQ.Ab8RN6-example" {
		t.Fatalf("test request = %+v, err %v", resp, err)
	}

	// No token yet: the test says so instead of sending an unauthorized request.
	_, err = p.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{
		Account: stored(TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil), fieldExpiresAt: expiredAt}, fullTarget),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("test without token: %v", err)
	}
	// The same for an expired one.
	_, err = p.BuildTestRequest(context.Background(), &pluginv1.BuildTestRequestRequest{
		Account: stored(TypeServiceAccount, map[string]any{"service_account_json": saJSON(nil), fieldAccessToken: "ya29.old", fieldExpiresAt: testNow.Unix() - 60}, fullTarget),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("test with an expired token: %v", err)
	}
}

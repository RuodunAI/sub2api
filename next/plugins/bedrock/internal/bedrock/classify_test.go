package bedrock

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// awsErrorAnswer is an error answer of the Bedrock runtime: the type in the
// x-amzn-errortype header, the message in the body.
func awsErrorAnswer(typ, message string) (map[string]string, []byte) {
	return map[string]string{"x-amzn-errortype": typ, "content-type": "application/json"}, []byte(mustJSON(map[string]string{"message": message}))
}

// TestClassifyError is the table of DESIGN-2026-11-17-CLOUD-PROVIDERS §9.4,
// row by row, with the decisions taken on it: a 403 AccessDenied cools the
// account down instead of disabling it, and a failure of the signature's
// format or time leaves the account alone.
func TestClassifyError(t *testing.T) {
	const (
		ret      = pluginv1.ClassifyErrorResponse_ACTION_RETURN_TO_CLIENT
		failover = pluginv1.ClassifyErrorResponse_ACTION_FAILOVER
		none     = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_UNSPECIFIED
		cooldown = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_COOLDOWN
		disable  = pluginv1.ClassifyErrorResponse_ACCOUNT_EFFECT_DISABLE
	)
	const (
		mismatch = "The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method."
		expired  = "Signature expired: 20261117T010000Z is now earlier than 20261117T011000Z (20261117T011500Z - 5 min.)"
		early    = "Signature not yet current: 20261117T020000Z is still later than 20261117T011500Z (20261117T011000Z + 5 min.)"
	)
	for _, tc := range []struct {
		name string
		// protocol defaults to anthropic.messages.
		protocol string
		status   int32
		// typ and message make an AWS error answer; body replaces it.
		typ, message string
		body         string
		headers      map[string]string
		transport    string
		wantAction   pluginv1.ClassifyErrorResponse_Action
		wantEffect   pluginv1.ClassifyErrorResponse_AccountEffect
		// wantCooldown is the cooldown in seconds from testNow (0: none).
		wantCooldown int64
		wantType     string
		// wantReason is a part of the reason shown on the account row.
		wantReason string
	}{
		// 429: fail over, cool down.
		{name: "429 ThrottlingException", status: 429, typ: "ThrottlingException", message: "Too many requests, please wait before trying again.",
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "rate_limit_error", wantReason: "rate limited"},
		{name: "429 ThrottlingException with retry-after", status: 429, typ: "ThrottlingException", message: "Too many requests", headers: map[string]string{"Retry-After": "7"},
			wantAction: failover, wantEffect: cooldown, wantCooldown: 7, wantType: "rate_limit_error", wantReason: "retry-after"},
		{name: "429 ModelNotReadyException", status: 429, typ: "ModelNotReadyException", message: "The model is not ready for inference.",
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "rate_limit_error", wantReason: "[ModelNotReadyException]"},

		// 503, 529, 500, 408: fail over, the account is left alone.
		{name: "503 ServiceUnavailableException", status: 503, typ: "ServiceUnavailableException", message: "Service unavailable",
			wantAction: failover, wantEffect: none, wantType: "api_error", wantReason: "upstream server error (503)"},
		{name: "529", status: 529, body: `{"message":"overloaded"}`, wantAction: failover, wantEffect: none, wantType: "overloaded_error", wantReason: "(529)"},
		{name: "500 InternalServerException", status: 500, typ: "InternalServerException", message: "Internal error",
			wantAction: failover, wantEffect: none, wantType: "api_error", wantReason: "(500)"},
		{name: "408 ModelTimeoutException", status: 408, typ: "ModelTimeoutException", message: "Model timed out",
			wantAction: failover, wantEffect: none, wantType: "invalid_request_error", wantReason: "[ModelTimeoutException]"},

		// 424, 400, 404: the request's own failure, back to the client.
		{name: "424 ModelErrorException", status: 424, typ: "ModelErrorException", message: "The model returned an error", wantAction: ret, wantEffect: none, wantType: "invalid_request_error"},
		{name: "400 ValidationException", status: 400, typ: "ValidationException", message: "Malformed input request: #: extraneous key [custom] is not permitted",
			wantAction: ret, wantEffect: none, wantType: "invalid_request_error"},
		{name: "400 without a type", status: 400, body: `{"message":"bad request"}`, wantAction: ret, wantEffect: none, wantType: "invalid_request_error"},
		{name: "404 ResourceNotFoundException", status: 404, typ: "ResourceNotFoundException", message: "Could not resolve the foundation model",
			wantAction: ret, wantEffect: none, wantType: "not_found_error"},

		// 403 that proves the key bad: disable.
		{name: "403 UnrecognizedClientException", status: 403, typ: "UnrecognizedClientException", message: "The security token included in the request is invalid.",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "upstream rejected credentials (403) [UnrecognizedClientException]"},
		{name: "403 InvalidClientTokenId", status: 403, typ: "InvalidClientTokenId", message: "The security token included in the request is invalid.",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "upstream rejected credentials"},
		{name: "403 InvalidSignatureException, signature mismatch", status: 403, typ: "InvalidSignatureException", message: mismatch,
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "upstream rejected credentials"},
		{name: "403 ExpiredTokenException", status: 403, typ: "ExpiredTokenException", message: "The security token included in the request is expired",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "temporary credentials expired"},
		{name: "403 ExpiredToken without the suffix", status: 403, typ: "ExpiredToken", message: "The security token included in the request is expired",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "temporary credentials expired"},
		{name: "401", status: 401, body: `{"message":"Unauthorized"}`, wantAction: failover, wantEffect: disable, wantType: "authentication_error", wantReason: "upstream rejected credentials (401)"},

		// The node's clock or the signature's format: not the account.
		{name: "403 InvalidSignatureException, signature expired", status: 403, typ: "InvalidSignatureException", message: expired,
			wantAction: failover, wantEffect: none, wantType: "permission_error", wantReason: "node's clock"},
		{name: "403 InvalidSignatureException, not yet current", status: 403, typ: "InvalidSignatureException", message: early,
			wantAction: failover, wantEffect: none, wantType: "permission_error", wantReason: "node's clock"},
		{name: "400 RequestExpired", status: 400, typ: "RequestExpired", message: "Request has expired.",
			wantAction: failover, wantEffect: none, wantType: "invalid_request_error", wantReason: "node's clock"},
		{name: "400 IncompleteSignatureException", status: 400, typ: "IncompleteSignatureException", message: "Authorization header requires 'Credential' parameter.",
			wantAction: failover, wantEffect: none, wantType: "invalid_request_error", wantReason: "the account is not at fault"},
		{name: "400 IncompleteSignature without the suffix", status: 400, typ: "IncompleteSignature", message: "Authorization header requires 'Signature' parameter.",
			wantAction: failover, wantEffect: none, wantType: "invalid_request_error", wantReason: "the account is not at fault"},

		// 403 AccessDenied and any other 403: cool down, never disable.
		{name: "403 AccessDeniedException", status: 403, typ: "AccessDeniedException", message: "You don't have access to the model with the specified model ID.",
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "permission_error", wantReason: "upstream denied access (403) [AccessDeniedException]"},
		{name: "403 of an unknown type", status: 403, body: `{"message":"Forbidden"}`,
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "permission_error", wantReason: "upstream denied access"},
		{name: "403 from a proxy's block page", status: 403, body: "<html><body>Access denied</body></html>", headers: map[string]string{"content-type": "text/html"},
			wantAction: failover, wantEffect: none, wantType: "permission_error", wantReason: "edge blocked"},

		// Where the type is read from.
		{name: "type with a URL after it", status: 403, typ: "ExpiredTokenException:http://internal.amazon.com/coral/com.amazon.coral.service/", message: "expired",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "[ExpiredTokenException]"},
		{name: "type in the body", status: 403, body: `{"__type":"com.amazon.coral.service#ExpiredTokenException","message":"expired"}`,
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "[ExpiredTokenException]"},
		{name: "type in the body, capital Message", status: 403, body: `{"__type":"AccessDeniedException","Message":"not authorized to perform: bedrock:InvokeModel"}`,
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "permission_error", wantReason: "not authorized to perform"},
		{name: "the header wins over the body", status: 403, typ: "AccessDeniedException", body: `{"__type":"ExpiredTokenException","message":"x"}`,
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "permission_error", wantReason: "[AccessDeniedException]"},

		// The OpenAI-compatible endpoints: same rules, OpenAI's vocabulary.
		{name: "openai: 429 in AWS shape", protocol: ProtocolChat, status: 429, typ: "ThrottlingException", message: "Too many requests",
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "rate_limit_error"},
		{name: "openai: 429 in OpenAI shape", protocol: ProtocolChat, status: 429, body: `{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}`,
			headers: map[string]string{"retry-after-ms": "2500"}, wantAction: failover, wantEffect: cooldown, wantCooldown: 2, wantType: "rate_limit_exceeded", wantReason: "Rate limit reached"},
		{name: "openai: 400 in OpenAI shape", protocol: ProtocolResponses, status: 400, body: `{"error":{"message":"Unsupported parameter","type":"invalid_request_error","code":"unsupported_parameter"}}`,
			wantAction: ret, wantEffect: none, wantType: "invalid_request_error"},
		{name: "openai: 403 AccessDeniedException", protocol: ProtocolChat, status: 403, typ: "AccessDeniedException", message: "no access",
			wantAction: failover, wantEffect: cooldown, wantCooldown: 60, wantType: "permission_error"},
		{name: "openai: 403 ExpiredTokenException", protocol: ProtocolResponses, status: 403, typ: "ExpiredTokenException", message: "expired",
			wantAction: failover, wantEffect: disable, wantType: "permission_error", wantReason: "temporary credentials expired"},
		{name: "openai: 401 in OpenAI shape", protocol: ProtocolChat, status: 401, body: `{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantAction: failover, wantEffect: disable, wantType: "invalid_request_error", wantReason: "[invalid_api_key]"},
		{name: "openai: 500", protocol: ProtocolChat, status: 500, typ: "InternalServerException", message: "Internal error", wantAction: failover, wantEffect: none, wantType: "server_error"},

		{name: "transport error", transport: "connection reset by peer", wantAction: failover, wantEffect: cooldown, wantCooldown: 10, wantType: "api_error", wantReason: "connection reset"},
		{name: "openai: transport error", protocol: ProtocolChat, transport: "EOF", wantAction: failover, wantEffect: cooldown, wantCooldown: 10, wantType: "server_error"},
	} {
		for _, typ := range []string{TypeAPIKey, TypeAccessKey, TypeSTS} {
			t.Run(tc.name+"/"+typ, func(t *testing.T) {
				headers, body := tc.headers, []byte(tc.body)
				if tc.typ != "" {
					h, b := awsErrorAnswer(tc.typ, tc.message)
					for k, v := range tc.headers {
						h[k] = v
					}
					headers = h
					if tc.body == "" {
						body = b
					}
				}
				protocol := tc.protocol
				if protocol == "" {
					protocol = ProtocolMessages
				}
				resp, err := newTestPlugin(Endpoints{}).ClassifyError(context.Background(), &pluginv1.ClassifyErrorRequest{
					Meta:    &pluginv1.RequestMeta{Protocol: protocol},
					Account: &pluginv1.Account{Type: typ},
					Status:  tc.status, Headers: headers, BodyPrefix: body, TransportError: tc.transport,
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
				if !strings.Contains(resp.GetReason(), tc.wantReason) {
					t.Fatalf("reason %q lacks %q", resp.GetReason(), tc.wantReason)
				}
				if tc.message != "" && tc.body == "" && resp.GetClientMessage() != tc.message {
					t.Fatalf("client message = %q", resp.GetClientMessage())
				}
			})
		}
	}
}

func TestShortType(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"ThrottlingException":          "ThrottlingException",
		" ThrottlingException ":        "ThrottlingException",
		"ThrottlingException:http://x": "ThrottlingException",
		"com.amazon.coral.service#ExpiredTokenException":   "ExpiredTokenException",
		"com.amazonaws.bedrock#AccessDeniedException:http": "AccessDeniedException",
	} {
		if got := shortType(in); got != want {
			t.Errorf("shortType(%q) = %q, want %q", in, got, want)
		}
	}
}

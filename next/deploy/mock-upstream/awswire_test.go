package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Frames recorded from aws-sdk-go-v2's eventstream.Encoder (v1.7.4): the
// mock's encoder writes the same bytes.
func TestEventStreamEncoderMatchesTheSDK(t *testing.T) {
	for _, tc := range []struct {
		headers [][2]string
		payload string
		hex     string
	}{
		{[][2]string{{":exception-type", "throttlingException"}, {":content-type", "application/json"}, {":message-type", "exception"}},
			`{"message":"Too many requests, please wait before trying again."}`,
			"000000b2000000613590d5d30f3a657863657074696f6e2d747970650700137468726f74746c696e67457863657074696f6e0d3a636f6e74656e742d747970650700106170706c69636174696f6e2f6a736f6e0d3a6d6573736167652d74797065070009657863657074696f6e7b226d657373616765223a22546f6f206d616e792072657175657374732c20706c656173652077616974206265666f726520747279696e6720616761696e2e227de8ffb26a"},
		{nil, `{"k":"v"}`, "000000190000000008d22a9a7b226b223a2276227dfd3a2ac3"},
	} {
		got := appendEventStreamMessage([]byte("x"), tc.headers, []byte(tc.payload))
		if hex.EncodeToString(got[1:]) != tc.hex || got[0] != 'x' {
			t.Errorf("payload %s:\n got %x\nwant %s", tc.payload, got[1:], tc.hex)
		}
	}
}

// eventStreamMessage is one decoded message of a test's response.
type eventStreamMessage struct {
	headers map[string]string
	payload []byte
}

// readEventStream decodes a whole event stream, checking both checksums of
// every message.
func readEventStream(t *testing.T, data []byte) []eventStreamMessage {
	t.Helper()
	var out []eventStreamMessage
	for len(data) > 0 {
		if len(data) < 16 {
			t.Fatalf("truncated message: %d bytes left", len(data))
		}
		total, headersLen := int(binary.BigEndian.Uint32(data)), int(binary.BigEndian.Uint32(data[4:]))
		if total > len(data) || 16+headersLen > total {
			t.Fatalf("bad lengths %d/%d in %d bytes", total, headersLen, len(data))
		}
		if crc32.ChecksumIEEE(data[:8]) != binary.BigEndian.Uint32(data[8:]) || crc32.ChecksumIEEE(data[:total-4]) != binary.BigEndian.Uint32(data[total-4:]) {
			t.Fatal("checksum mismatch")
		}
		msg := eventStreamMessage{headers: map[string]string{}, payload: data[12+headersLen : total-4]}
		for h := data[12 : 12+headersLen]; len(h) > 0; {
			name := string(h[1 : 1+h[0]])
			h = h[1+h[0]:]
			if h[0] != 7 {
				t.Fatalf("header %s has type %d", name, h[0])
			}
			n := int(binary.BigEndian.Uint16(h[1:]))
			msg.headers[name], h = string(h[3:3+n]), h[3+n:]
		}
		out, data = append(out, msg), data[total:]
	}
	return out
}

// Signatures recorded from aws-sdk-go-v2 (aws/signer/v4, v1.41.0) at a fixed
// time with AWS's example credentials: the mock's check accepts them and
// rejects the same request once anything signed changes.
func TestSigV4CheckAcceptsSDKSignatures(t *testing.T) {
	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		body   = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":64,"messages":[{"role":"user","content":"hello ✓"}]}`
	)
	now := time.Date(2026, 11, 17, 8, 9, 10, 0, time.UTC)
	for _, tc := range []struct {
		name, method, url, body, token, authorization string
	}{
		{"model id with a colon", "POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke", body, "",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=645c5fc40eee19b7238b51e86e5189e9d803c3749b1412d6d28ba11eba163f8d"},
		{"arn", "POST", "https://bedrock-runtime.us-west-2.amazonaws.com/model/arn%3Aaws%3Abedrock%3Aus-west-2%3A123456789012%3Aapplication-inference-profile%2Fabc123def456/invoke", body, "",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-west-2/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=92346d80ca4d237e8f93793a3b89fece929ced4d15127fadf8d50058ab190a44"},
		{"session token", "POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream", body,
			"IQoJb3JpZ2luX2VjEXAMPLETOKEN//////////wEaCXVzLWVhc3QtMSJHMEUCIQD+/==",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-security-token, Signature=2be461a9cb708c9207c321108c1310fc893916ed483d09a2f4a6d0aeb91929fa"},
		{"query", "GET", "https://bedrock.us-east-1.amazonaws.com/foundation-models?byProvider=anthropic&z=a%20b&a=%E2%9C%93", "", "",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=host;x-amz-date, Signature=f89f3cbdf464acebc617c39fea10340c612248d279077b34f7633902c73db8bb"},
		{"host with a port", "POST", "http://127.0.0.1:8080/model/m%3A0/invoke", body, "",
			"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261117/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=d3286cfec1a49096c1ea07945ec3da279a8cc38181fd7926f40fcb09397359fb"},
	} {
		req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
		req.Header.Set("Authorization", tc.authorization)
		req.Header.Set("X-Amz-Date", "20261117T080910Z")
		if tc.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if tc.token != "" {
			req.Header.Set("X-Amz-Security-Token", tc.token)
		}
		payload := []byte(tc.body)
		if err := checkSigV4(req, payload, secret, now); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if err := checkSigV4(req, payload, secret, now.Add(16*time.Minute)); err == nil {
			t.Errorf("%s: accepted 16 minutes later", tc.name)
		}
		if err := checkSigV4(req, payload, secret+"x", now); err == nil {
			t.Errorf("%s: accepted with another secret", tc.name)
		}
		if err := checkSigV4(req, append(bytes.Clone(payload), ' '), secret, now); err == nil {
			t.Errorf("%s: accepted with another body", tc.name)
		}
		req.URL.RawQuery += "&extra=1"
		if err := checkSigV4(req, payload, secret, now); err == nil {
			t.Errorf("%s: accepted with another query", tc.name)
		}
	}
}

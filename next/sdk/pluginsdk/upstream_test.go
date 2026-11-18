package pluginsdk

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

func TestUpstreamBuildsTheTwoHeaderKinds(t *testing.T) {
	body := []byte(`{"model":"m","stream":true}`)
	got, err := NewUpstream("", "https://api.example.com/v1/messages?beta=true").
		Header("Authorization", "Bearer k").
		Headers(http.Header{"X-Amz-Date": {"20261118T000000Z"}}).
		DefaultHeader("Anthropic-Version", "2023-06-01").
		DefaultHeaders(http.Header{"Accept": {"a", "b"}}).
		DefaultHeader("x-empty", "").
		Body(body).Model("m-upstream").ResponseStream("aws-eventstream-chunk").
		ResponseHeaderTimeout(1500 * time.Millisecond).TotalTimeout(time.Microsecond).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	want := &pluginv1.UpstreamRequest{
		Url:            "https://api.example.com/v1/messages?beta=true",
		Headers:        map[string]string{"authorization": "Bearer k", "x-amz-date": "20261118T000000Z"},
		DefaultHeaders: map[string]string{"anthropic-version": "2023-06-01", "accept": "a, b"},
		Body:           body,
		UpstreamModel:  "m-upstream",
		ResponseStream: "aws-eventstream-chunk",
		Options:        &pluginv1.UpstreamOptions{ResponseHeaderTimeoutMs: 1500, TotalTimeoutMs: 1},
	}
	if !proto.Equal(got, want) {
		t.Fatalf("built %v\nwant  %v", got, want)
	}
}

func TestUpstreamBuildReportsWhatTheHostRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		build *Upstream
		want  string
	}{
		"relative url":       {NewUpstream("", "/v1/messages"), "not an absolute"},
		"other scheme":       {NewUpstream("", "ftp://example.com/x"), "not an absolute"},
		"content-length":     {NewUpstream("", "https://example.com").Header("Content-Length", "3"), `"content-length" belongs to the host's transport`},
		"accept-encoding":    {NewUpstream("", "https://example.com").DefaultHeader("Accept-Encoding", "gzip"), `"accept-encoding" belongs to the host's transport`},
		"connection":         {NewUpstream("", "https://example.com").DefaultHeader("connection", "close"), "transport"},
		"transfer-encoding":  {NewUpstream("", "https://example.com").Header("transfer-encoding", "chunked"), "transport"},
		"line break":         {NewUpstream("", "https://example.com").Header("x-a", "b\r\nx-c: d"), "malformed"},
		"negative timeout":   {NewUpstream("", "https://example.com").TotalTimeout(-time.Second), "negative"},
		"several at once":    {NewUpstream("", "nope").Header("connection", "x"), "transport"},
		"websocket is fine":  {NewUpstream("GET", "wss://example.com/v1/realtime"), ""},
		"huge timeout is ok": {NewUpstream("", "https://example.com").TotalTimeout(1 << 62), ""},
	} {
		got, err := tc.build.Build()
		if tc.want == "" {
			if err != nil || got == nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || got != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", name, err, tc.want)
		}
	}
}

// A request written by hand is held to the same rules as a built one, the
// lower-case names included.
func TestValidateUpstreamRequest(t *testing.T) {
	ok := &pluginv1.UpstreamRequest{Url: "https://example.com", Headers: map[string]string{"authorization": "x"}}
	if err := ValidateUpstreamRequest(ok); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]*pluginv1.UpstreamRequest{
		"nil":              nil,
		"mixed case":       {Url: "https://example.com", Headers: map[string]string{"Authorization": "x"}},
		"mixed default":    {Url: "https://example.com", DefaultHeaders: map[string]string{"X-A": "x"}},
		"empty name":       {Url: "https://example.com", Headers: map[string]string{"": "x"}},
		"transport header": {Url: "https://example.com", DefaultHeaders: map[string]string{"content-length": "1"}},
		"negative option":  {Url: "https://example.com", Options: &pluginv1.UpstreamOptions{ResponseHeaderTimeoutMs: -1}},
		"no host":          {Url: "https:///x"},
	} {
		if err := ValidateUpstreamRequest(r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRequestModel(t *testing.T) {
	meta := &pluginv1.RequestMeta{Model: "meta-model"}
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"body wins":     {`{"model":"body-model"}`, "body-model"},
		"no body field": {`{"input":"x"}`, "meta-model"},
		"empty string":  {`{"model":""}`, "meta-model"},
		"not a string":  {`{"model":7}`, "meta-model"},
		"no body":       {``, "meta-model"},
	} {
		if got := RequestModel(&pluginv1.ExecuteRequest{Meta: meta, Body: []byte(tc.body)}); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	if got := RequestModel(nil); got != "" {
		t.Errorf("nil request = %q", got)
	}
}

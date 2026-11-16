package check

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

func TestAccountStreamOnly(t *testing.T) {
	own := &manifest.Platform{ID: "mine", Endpoints: []manifest.Endpoint{
		{ID: "gen", Protocol: "mine.gen", Kind: manifest.EndpointKindProxy,
			Request: manifest.EndpointRequest{StreamPath: "stream"}, Response: manifest.EndpointResp{Stream: "sse", NonStream: "json"}},
		{ID: "json", Protocol: "mine.json", Kind: manifest.EndpointKindProxy, Response: manifest.EndpointResp{NonStream: "json"}},
		{ID: "always", Protocol: "mine.always", Kind: manifest.EndpointKindProxy,
			Request: manifest.EndpointRequest{Stream: true}, Response: manifest.EndpointResp{Stream: "sse"}},
	}}
	for _, tc := range []struct {
		name      string
		platform  string
		endpoints []string
		protos    []string
		tooling   bool
		code      string
	}{
		{"absent", "openai", nil, nil, false, ""},
		{"responses", "openai", nil, []string{"openai.responses"}, false, ""},
		{"chat and responses", "openai", nil, []string{"openai.chat", "openai.responses"}, false, ""},
		{"subset", "openai", []string{"responses", "responses_websocket"}, []string{"openai.responses"}, false, ""},
		{"empty", "openai", nil, []string{}, false, "required"},
		{"blank", "openai", nil, []string{" "}, false, "required"},
		{"duplicate", "openai", nil, []string{"openai.responses", "openai.responses"}, false, "duplicate"},
		{"other platform", "openai", nil, []string{"anthropic.messages"}, false, "unknown_protocol"},
		{"outside subset", "openai", []string{"responses"}, []string{"openai.chat"}, false, "unknown_protocol"},
		{"websocket", "openai", nil, []string{"openai.responses_ws"}, false, "not_streamable"},
		{"json only", "openai", nil, []string{"openai.embeddings"}, false, "not_streamable"},
		{"own", "mine", nil, []string{"mine.gen"}, false, ""},
		{"own without stream path", "mine", nil, []string{"mine.always"}, false, "not_streamable"},
		{"own json", "mine", nil, []string{"mine.json"}, false, "not_streamable"},
		{"external", "video", nil, []string{"video.generate"}, false, ""},
		{"missing platform", "missing", nil, []string{"missing.gen"}, false, "unknown_platform"},
		{"missing platform offline", "missing", nil, []string{"missing.gen"}, true, ""},
		{"missing platform offline foreign", "missing", nil, []string{"other.gen"}, true, "unknown_protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validator{opt: ValidateOptions{Tooling: tc.tooling, OtherPlatforms: []PlatformOwner{{ID: "video", Endpoints: []manifest.Endpoint{{
				ID: "generate", Protocol: "video.generate", Kind: manifest.EndpointKindProxy,
				Request: manifest.EndpointRequest{StreamPath: "stream"}, Response: manifest.EndpointResp{Stream: "sse"}}}}}}}
			var p *manifest.Platform
			if tc.platform == "mine" {
				p = own
			}
			v.accountStreamOnly("platforms[0]", manifest.AccountPlatform{Platform: tc.platform, Endpoints: tc.endpoints, StreamOnly: tc.protos}, p)
			if tc.code == "" {
				if len(v.errs) != 0 {
					t.Fatalf("unexpected errors: %+v", v.errs)
				}
				return
			}
			if len(v.errs) != 1 || v.errs[0].Code != tc.code {
				t.Fatalf("errors = %+v, want %s", v.errs, tc.code)
			}
		})
	}
}

func TestAccountPlatformStreamsOnly(t *testing.T) {
	ap := manifest.AccountPlatform{Platform: "openai", StreamOnly: []string{"openai.responses"}}
	if !ap.StreamsOnly("openai.responses") || ap.StreamsOnly("openai.chat") || (manifest.AccountPlatform{}).StreamsOnly("openai.responses") {
		t.Fatal("StreamsOnly")
	}
}

// accountPlatform runs the streamOnly check for every account type entry.
func TestAccountPlatformChecksStreamOnly(t *testing.T) {
	v := validator{}
	v.accountPlatform("accountTypes[0].platforms[0]", manifest.AccountPlatform{Platform: "openai", StreamOnly: []string{"openai.responses_ws"}}, map[string]bool{}, nil)
	if len(v.errs) != 1 || v.errs[0].Field != "accountTypes[0].platforms[0].streamOnly[0]" || v.errs[0].Code != "not_streamable" {
		t.Fatalf("errors = %+v", v.errs)
	}
}

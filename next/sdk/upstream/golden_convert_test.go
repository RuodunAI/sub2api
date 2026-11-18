package upstream

// DELETE WITH THE ENGINE. This file is the one-off conversion of the golden
// files into sdk/pluginsdk/standard/goldentest and the proof that the
// conversion is faithful (docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md
// §7.3, work package W2). It is removed together with this package; the
// converted cases and goldentest stay.
//
// The two tests derive the same thing from independent sources:
//
//   - TestGoldenConversion converts the recorded answers
//     (testdata/golden/*.json) without calling the engine and requires the
//     committed goldentest files to be exactly that output. Run it with
//     SUB2API_GOLDEN_CONVERT=write to write them.
//   - TestConvertedGoldenReplay replays every converted case through the
//     engine as it is today, the way the core uses it (the fields the core
//     reads from the body, Build, the patches applied to the body, the
//     headers split into the declaration's and the credential).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protojson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
)

const (
	convertedDir = "../pluginsdk/standard/goldentest/testdata/golden"
	convertEnv   = "SUB2API_GOLDEN_CONVERT"
)

// convertedCounts are the cases of each golden file; every one is converted.
// amendedCounts are those among them whose expected answer is today's and
// not the recorded one (convertCase): the calls openai (1) and gemini (8)
// answered for an account of another type, and relay's 40 successful
// account tests.
var (
	convertedCounts = map[string]int{"openai": 441, "anthropic": 343, "gemini": 371, "relay": 343}
	amendedCounts   = map[string]int{"openai": 1, "anthropic": 0, "gemini": 8, "relay": 40}
)

// credentialHeaders name the header each plugin's credential is sent in:
// the one header of an answer that becomes authoritative, every other one
// is a default the operator's header settings may change. Written out here
// rather than read from the profile so that the conversion does not use the
// engine; the replay checks it against Profile.AuthHeader.
var credentialHeaders = map[string]string{
	"openai": "authorization", "anthropic": "x-api-key", "gemini": "x-goog-api-key", "relay": "x-api-key",
}

// goldenRequestFields are the body fields the core hands to these account
// types (the platforms' and relay's requestFields): the model.
var goldenRequestFields = []string{"model"}

func TestGoldenConversion(t *testing.T) {
	if !slices.Equal(goldentest.Plugins(), []string{"anthropic", "gemini", "openai", "relay"}) {
		t.Fatalf("converted files = %v", goldentest.Plugins())
	}
	for plugin, gt := range goldenTypes {
		t.Run(plugin, func(t *testing.T) {
			var old goldenFile
			readGolden(t, filepath.Join("testdata", "golden", plugin+".json"), &old)
			out := goldentest.File{Plugin: old.Plugin, AccountType: old.AccountType, PassHeaders: gt.passHeaders}
			if old.Plugin != plugin || old.AccountType != gt.accountType {
				t.Fatalf("golden file is of %s/%s", old.Plugin, old.AccountType)
			}
			seen := map[string]bool{}
			for _, r := range old.Cases {
				if seen[r.Name] {
					t.Fatalf("case name %q is used twice", r.Name)
				}
				seen[r.Name] = true
				out.Cases = append(out.Cases, convertCase(t, plugin, r))
			}
			amended := 0
			for _, c := range out.Cases {
				if c.Note != "" {
					amended++
				}
			}
			if len(out.Cases) != convertedCounts[plugin] || amended != amendedCounts[plugin] {
				t.Fatalf("%d cases, %d amended, want %d and %d", len(out.Cases), amended, convertedCounts[plugin], amendedCounts[plugin])
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(convertedDir, plugin+".json")
			if os.Getenv(convertEnv) == "write" {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			frozen, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frozen, buf.Bytes()) {
				t.Fatalf("%s is not the conversion of testdata/golden/%s.json (the converted cases are frozen: do not edit them)", path, plugin)
			}
		})
	}
}

func readGolden(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// convertCase converts one recorded case. A build case becomes an execute
// case; the others keep their messages.
func convertCase(t *testing.T, plugin string, r goldenResult) goldentest.Case {
	t.Helper()
	c := goldentest.Case{Name: r.Name, Kind: r.Kind, Now: r.Now, Input: r.Input, Want: r.Output}
	if r.Error != nil {
		c.Want = nil
		c.Error = &goldentest.Error{Code: r.Error.Code, Message: goldentest.Unwrap(r.Error.Message)}
	}
	foreign := foreignAccountType(t, plugin, r)
	switch {
	case r.Kind == "build":
		c.Kind = goldentest.KindExecute
		in, want := convertBuild(t, plugin, r)
		c.Input = mustJSON(t, in)
		if c.Error == nil {
			c.Want = mustJSON(t, want)
		}
	}
	switch {
	case foreign != "" && (c.Error == nil || c.Error.Message != unsupportedType(foreign)):
		// The openai and gemini plugins answered some calls for an account
		// of another type as if it were theirs (or with another error
		// first). A profile is bound to one account type and refuses the
		// account, like the anthropic and relay plugins always did; that is
		// today's answer and the one recorded here. The core never hands a
		// plugin an account of a type it did not declare.
		c.Want, c.Error = nil, &goldentest.Error{Code: "FailedPrecondition", Message: unsupportedType(foreign)}
		c.Note = "not the recorded answer: the plugin did not refuse the account of another type, the declaration does"
	case plugin == "relay" && r.Kind == "test" && r.Error == nil:
		c.Note = "not the recorded answer: model and usage_protocol were empty before relay became a declaration"
		// The one answer that changed when relay became a declaration
		// (TestGolden): its account test names the model tested and the
		// protocol its usage is read with, like every other type's.
		var w map[string]any
		if err := json.Unmarshal(r.Output, &w); err != nil {
			t.Fatal(err)
		}
		body, _ := w["body_json"].(string)
		w["model"], w["usage_protocol"] = gjson.Get(body, "model").String(), "anthropic.messages"
		c.Want = mustJSON(t, w)
	}
	return c
}

// convertBuild turns a recorded BuildUpstreamRequest exchange into the
// complete request body the plugin is handed and the final request it must
// produce: the body is synthesized from the recorded fields and metadata
// (synthBody), the recorded patches are applied to it, and the recorded
// headers are split into the credential and the rest.
func convertBuild(t *testing.T, plugin string, r goldenResult) (*goldentest.ExecuteInput, *goldentest.UpstreamRequest) {
	t.Helper()
	req := &pluginv1.BuildUpstreamRequestRequest{}
	if err := protojson.Unmarshal(r.Input, req); err != nil {
		t.Fatalf("%s: input: %v", r.Name, err)
	}
	in := &goldentest.ExecuteInput{
		Meta:          goldentest.Meta{Protocol: req.GetMeta().GetProtocol(), Model: req.GetMeta().GetModel(), Stream: req.GetMeta().GetStream()},
		ClientHeaders: req.GetInboundHeaders(),
		Body:          synthBody(t, r.Name, req),
	}
	if a := req.GetAccount(); a != nil {
		in.Account = &goldentest.Account{ID: a.GetId(), Type: a.GetType(), CredentialsJSON: a.GetCredentialsJson(), SettingsJSON: a.GetSettingsJson()}
	}
	// Nothing of the recorded input may be lost: built back, it is the input.
	if back := buildRequest(in); !protoEqualJSON(t, back, req) {
		t.Fatalf("%s: the input does not survive the conversion:\n got %v\nwant %v", r.Name, back, req)
	}
	if r.Error != nil {
		return in, nil
	}
	resp := &pluginv1.BuildUpstreamRequestResponse{}
	if err := protojson.Unmarshal(r.Output, resp); err != nil {
		t.Fatalf("%s: output: %v", r.Name, err)
	}
	if resp.GetSigning() != nil || resp.GetResponseStream() != "" {
		t.Fatalf("%s: the answer signs or reframes, which the conversion does not carry", r.Name)
	}
	return in, finalRequest(t, r.Name, in.Body, resp, credentialHeaders[plugin])
}

// synthBody is the smallest request body the core could have read the
// recorded input from: each recorded field at its path, and the stream flag
// at the endpoint's streamPath when the metadata says the request streams
// (the core derives meta.stream from that flag). Without fields it is {}.
func synthBody(t *testing.T, name string, req *pluginv1.BuildUpstreamRequestRequest) string {
	t.Helper()
	body := "{}"
	var err error
	for _, path := range goldenRequestFields {
		raw, ok := req.GetFields()[path]
		if !ok {
			continue
		}
		if body, err = sjson.SetRaw(body, path, raw); err != nil {
			t.Fatalf("%s: field %s: %v", name, path, err)
		}
	}
	if path := streamPaths()[req.GetMeta().GetProtocol()]; path != "" && req.GetMeta().GetStream() {
		if body, err = sjson.SetRaw(body, path, "true"); err != nil {
			t.Fatalf("%s: stream flag: %v", name, err)
		}
	}
	return body
}

// streamPaths are the body paths of the stream flag by protocol, from the
// built-in platform definitions.
func streamPaths() map[string]string {
	out := map[string]string{}
	for _, p := range platforms.Builtin() {
		for _, e := range p.Endpoints {
			if e.Request.StreamPath != "" {
				out[e.Protocol] = e.Request.StreamPath
			}
		}
	}
	return out
}

// buildRequest is the BuildUpstreamRequest the core sends for in: the
// request fields it reads from the body, as gateway.requestFieldValues does.
func buildRequest(in *goldentest.ExecuteInput) *pluginv1.BuildUpstreamRequestRequest {
	req := &pluginv1.BuildUpstreamRequestRequest{Meta: in.Meta.Proto(), Account: in.Account.Proto(), InboundHeaders: in.ClientHeaders}
	for _, path := range goldenRequestFields {
		if v := gjson.Get(in.Body, path); v.Exists() {
			if req.Fields == nil {
				req.Fields = map[string]string{}
			}
			req.Fields[path] = v.Raw
		}
	}
	return req
}

// finalRequest is what the core sends for a built answer: its patches
// applied to body in order (gateway.applyPatches), the credential header as
// the authoritative header and every other header as a default
// (gateway.forwardBuilt: the declaration's headers go under the operator's
// header settings, the declared auth header is written again after them).
func finalRequest(t *testing.T, name, body string, resp *pluginv1.BuildUpstreamRequestResponse, credential string) *goldentest.UpstreamRequest {
	t.Helper()
	var err error
	for _, p := range resp.GetPatches() {
		switch p.GetOp() {
		case pluginv1.BodyPatch_OP_SET:
			body, err = sjson.SetRaw(body, p.GetPath(), p.GetValueJson())
		case pluginv1.BodyPatch_OP_DELETE:
			body, err = sjson.Delete(body, p.GetPath())
		default:
			err = fmt.Errorf("unknown op %v", p.GetOp())
		}
		if err != nil {
			t.Fatalf("%s: patch %s: %v", name, p.GetPath(), err)
		}
	}
	out := &goldentest.UpstreamRequest{Method: resp.GetMethod(), URL: resp.GetUrl(), Body: body, UpstreamModel: resp.GetUpstreamModel()}
	for k, v := range resp.GetHeaders() {
		h := &out.DefaultHeaders
		if credential != "" && k == credential {
			h = &out.Headers
		}
		if *h == nil {
			*h = map[string]string{}
		}
		(*h)[k] = v
	}
	if credential != "" && out.Headers == nil {
		t.Fatalf("%s: the answer has no %s header", name, credential)
	}
	return out
}

// foreignAccountType is the account type of a case whose input carries an
// account of a type other than the golden file's; "" otherwise. Credential
// validation names the type outside an account and answers it itself.
func foreignAccountType(t *testing.T, plugin string, r goldenResult) string {
	t.Helper()
	var in struct {
		Account struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if err := json.Unmarshal(r.Input, &in); err != nil {
		t.Fatalf("%s: input: %v", r.Name, err)
	}
	if in.Account.Type == "" || in.Account.Type == goldenTypes[plugin].accountType {
		return ""
	}
	return in.Account.Type
}

func unsupportedType(typ string) string { return fmt.Sprintf("unsupported account type %q", typ) }

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func protoEqualJSON(t *testing.T, a, b *pluginv1.BuildUpstreamRequestRequest) bool {
	t.Helper()
	return bytes.Equal(marshalGolden(t, a), marshalGolden(t, b))
}

// TestConvertedGoldenReplay replays the converted cases through the engine.
// Every case must pass, the account type mismatches TestGolden skips
// included.
func TestConvertedGoldenReplay(t *testing.T) {
	for plugin, gt := range goldenTypes {
		t.Run(plugin, func(t *testing.T) {
			at := &manifest.AccountType{ID: gt.accountType, Upstream: &manifest.Upstream{}}
			if err := json.Unmarshal([]byte(gt.upstream), at.Upstream); err != nil {
				t.Fatal(err)
			}
			p, err := Resolve(at, gt.passHeaders)
			if err != nil {
				t.Fatal(err)
			}
			if p.AuthHeader() != credentialHeaders[plugin] {
				t.Fatalf("credential header %q, the conversion used %q", p.AuthHeader(), credentialHeaders[plugin])
			}
			f, err := goldentest.Load(plugin)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Cases) != convertedCounts[plugin] || !slices.Equal(f.PassHeaders, gt.passHeaders) || f.AccountType != gt.accountType {
				t.Fatalf("%d cases of %s with pass headers %v", len(f.Cases), f.AccountType, f.PassHeaders)
			}
			goldentest.Run(t, plugin, engineSubject(t, p))
		})
	}
}

// engineSubject is the engine behind the entry points goldentest replays.
func engineSubject(t *testing.T, p *Profile) goldentest.Subject {
	return goldentest.Subject{
		Execute: func(_ context.Context, in *goldentest.ExecuteInput) (*goldentest.UpstreamRequest, error) {
			resp, err := p.Build(buildRequest(in))
			if err != nil {
				return nil, err
			}
			return finalRequest(t, "replay", in.Body, resp, p.AuthHeader()), nil
		},
		Test: func(_ context.Context, in *pluginv1.BuildTestRequestRequest) (*pluginv1.BuildTestRequestResponse, error) {
			return p.Test(in)
		},
		Models: func(_ context.Context, in *pluginv1.BuildModelsRequestRequest) (*pluginv1.BuildModelsRequestResponse, error) {
			return p.Models(in)
		},
		Validate: func(_ context.Context, in *pluginv1.ValidateCredentialsRequest) (*pluginv1.ValidateCredentialsResponse, error) {
			return p.Validate(in), nil
		},
		Classify: func(_ context.Context, now time.Time, in *pluginv1.ClassifyErrorRequest) (*pluginv1.ClassifyErrorResponse, error) {
			return p.Classify(now, in), nil
		},
	}
}

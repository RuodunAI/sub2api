package openai

import (
	"bytes"
	"encoding/json"
	"os"
	"path"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
	"github.com/Sub2API-Devs/sup2api/next/sdk/upstream"
)

// The package and its one account type. replayed is the upstream
// sdk/upstream's golden replay (TestGolden) proves to answer exactly like the
// plugin code it replaced; declaration is the type's upstream, which adds
// the account's organization and project to it (TestOrganizationAndProject
// keeps the two in step for an account that sets neither).
const (
	key         = "openai"
	version     = "0.4.4"
	platform    = "openai"
	replayed    = `{"preset":"openai","filterFields":[]}`
	declaration = `{
		"preset": "openai",
		"filterFields": [],
		"credentials": {"fields": ["organization", "project"]},
		"headers": {
			"openai-organization": {"set": "{credentials.organization}"},
			"openai-project": {"set": "{credentials.project}"}
		}
	}`
)

// TestManifest runs manifest.json through the checks the core runs before
// it installs a package (sdk/manifest/check in tooling mode, which also
// resolves the upstream declaration with the core's engine) and pins what
// the move to runtime "none" keeps: the identity, the account type of the
// built-in platform and its replayed declaration.
func TestManifest(t *testing.T) {
	m := load(t)
	if m.Key != key || m.Version != version || m.Publisher != "sub2api" || m.Runtime != manifest.RuntimeNone {
		t.Fatalf("identity = %s %s %s %s", m.Key, m.Version, m.Publisher, m.Runtime)
	}
	if len(m.AccountTypes) != 1 {
		t.Fatalf("accountTypes = %+v", m.AccountTypes)
	}
	at := m.AccountTypes[0]
	if at.ID != "apikey" || len(at.Platforms) != 1 || at.Platforms[0].Platform != platform {
		t.Fatalf("account type %s platforms = %+v, want apikey on %s", at.ID, at.Platforms, platform)
	}
	assertJSON(t, at.Upstream.Layer(), declaration)
	p := resolve(t, at, nil)
	// base_url is guarded (CONTRACTS §21.3): without account:settings:custom
	// only the default upstream may be used.
	if gs := at.GuardedSettings; len(gs) != 1 || gs[0].Field != "base_url" || len(gs[0].Allowed) != 1 || gs[0].Allowed[0] != p.Upstream().BaseURL.Default {
		t.Fatalf("guardedSettings = %+v, want base_url -> [%s]", gs, p.Upstream().BaseURL.Default)
	}
}

// clientHeaders are what a client of the two OpenAI SDK headers sends.
var clientHeaders = map[string]string{"openai-organization": "org-client", "openai-project": "proj_client", "user-agent": "cli/1"}

// TestOrganizationAndProject covers the account's organization and project
// settings on every route, the WebSocket handshake included: a value set on
// the account replaces the client's header, and an account that sets
// neither (absent or left empty in the form) is answered exactly like the
// replayed declaration, so the client's own headers still pass through.
func TestOrganizationAndProject(t *testing.T) {
	at := load(t).AccountTypes[0]
	passHeaders := at.PassHeaders(builtinPlatform)
	declared := resolve(t, at, passHeaders)
	base := at
	base.Upstream = &manifest.Upstream{}
	if err := json.Unmarshal([]byte(replayed), base.Upstream); err != nil {
		t.Fatal(err)
	}
	baseline := resolve(t, base, passHeaders)

	for protocol := range declared.Upstream().Routes {
		for _, inbound := range []map[string]string{nil, clientHeaders} {
			for _, settings := range []string{"", `{"organization":"","project":""}`} {
				got, want := build(t, declared, protocol, settings, inbound), build(t, baseline, protocol, "", inbound)
				if !proto.Equal(got, want) {
					t.Errorf("%s, settings %q, client headers %v:\n got %v\nwant %v", protocol, settings, inbound, got, want)
				}
			}
			for settings, want := range map[string][2]string{
				`{"organization":"org-acct","project":"proj_acct"}`: {"org-acct", "proj_acct"},
				`{"organization":"org-acct"}`:                       {"org-acct", inbound["openai-project"]},
				`{"project":"proj_acct"}`:                           {inbound["openai-organization"], "proj_acct"},
			} {
				h := build(t, declared, protocol, settings, inbound).Headers
				if got := [2]string{h["openai-organization"], h["openai-project"]}; got != want {
					t.Errorf("%s, settings %s, client headers %v: organization, project = %q, want %q", protocol, settings, inbound, got, want)
				}
			}
		}
	}
}

func builtinPlatform(id string) *manifest.Platform {
	for _, p := range platforms.Builtin() {
		if p.ID == id {
			return &p
		}
	}
	return nil
}

func resolve(t *testing.T, at manifest.AccountType, passHeaders []string) *upstream.Profile {
	t.Helper()
	p, err := upstream.Resolve(&at, passHeaders)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// build is the upstream request of protocol for an account with settings.
func build(t *testing.T, p *upstream.Profile, protocol, settings string, inbound map[string]string) *pluginv1.BuildUpstreamRequestResponse {
	t.Helper()
	out, err := p.Build(&pluginv1.BuildUpstreamRequestRequest{
		Meta:           &pluginv1.RequestMeta{Protocol: protocol, Model: "gpt-4o-mini", Stream: true},
		Account:        &pluginv1.Account{Id: 1, Type: "apikey", CredentialsJson: `{"api_key":"sk-test-0123456789"}`, SettingsJson: settings},
		InboundHeaders: inbound,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// load decodes manifest.json strictly and checks it with the files a
// package carries.
func load(t *testing.T) manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	files := map[string][]byte{"manifest.json": raw}
	for _, dir := range []string{"forms", "assets"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			if files[p], err = os.ReadFile(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := check.Validate(&m, files, check.ValidateOptions{Tooling: true}); err != nil {
		t.Fatal(err)
	}
	return m
}

// assertJSON compares two JSON documents by value.
func assertJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal([]byte(want), &w)
	if g == nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("upstream = %s, want %s", got, want)
	}
}

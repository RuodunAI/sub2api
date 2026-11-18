package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/sdk/platforms"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"

	"github.com/Sub2API-Devs/sup2api/next/plugins/azure-openai/internal/azureopenai"
)

// endpoints are the openai endpoints each type serves; the type's routes
// must be exactly their protocols (TestManifest).
var endpoints = map[string][]string{
	typeV1:         {"chat_completions", "responses", "embeddings"},
	typeDeployment: {"chat_completions", "embeddings"},
}

// TestManifest runs manifest.json through the checks the core runs before
// it installs a package and pins it: a gRPC process on the forwarding v2
// core, account types only, one creation group whose first variant is the
// v1 API, an unguarded resource endpoint (as for relay, CONTRACTS §21.3.1),
// no prefilled models (a model is a deployment name), and the credentials
// of its own accounts, which the plugin needs to build their requests.
func TestManifest(t *testing.T) {
	m := load(t)
	if m.Key != "azure_openai" || m.Version != "0.1.0" || m.Publisher != "sub2api" || m.Runtime != manifest.RuntimeGRPC || m.HostCompat != ">=0.1.304 <0.2.0" {
		t.Fatalf("identity = %s %s %s %s %s", m.Key, m.Version, m.Publisher, m.Runtime, m.HostCompat)
	}
	if m.Entry.GRPC == nil || m.Entry.GRPC.Binaries != "runtimes/{os}-{arch}/plugin" {
		t.Errorf("entry = %+v", m.Entry)
	}
	if !slices.ContainsFunc(m.Capabilities, func(c manifest.Capability) bool { return c.ID == manifest.CapPlatformAdapter }) {
		t.Errorf("capabilities = %+v", m.Capabilities)
	}
	if i := slices.IndexFunc(m.HostPermissions, func(p manifest.HostPermission) bool { return p.ID == "accounts.credentials" }); i < 0 || m.HostPermissions[i].Scope["types"] != "own" {
		t.Errorf("hostPermissions = %+v, want accounts.credentials on its own types", m.HostPermissions)
	}
	if len(m.Platforms) != 0 || len(m.AccountTypes) != 2 || m.AccountTypes[0].ID != typeV1 || m.AccountTypes[1].ID != typeDeployment {
		t.Fatalf("platforms = %+v accountTypes = %+v, want %s then %s only", m.Platforms, m.AccountTypes, typeV1, typeDeployment)
	}
	// The client headers the core hands over are the openai platform's
	// minus openai-organization and openai-project, which name an
	// organization and a project at OpenAI itself and mean nothing to
	// Azure; derived from the platform, so a header it gains later is
	// noticed.
	notForwarded := []string{"openai-organization", "openai-project"}
	fromPlatform := slices.DeleteFunc(slices.Clone(builtinPlatform("openai").PassHeaders), func(h string) bool { return slices.Contains(notForwarded, h) })
	types := map[string]standard.Type{typeV1: azureopenai.V1(), typeDeployment: azureopenai.Deployment()}
	for _, at := range m.AccountTypes {
		if at.CreationGroup != "azure-openai" || at.AuthMethodLabel["en"] == "" || at.AuthMethodLabel["zh"] == "" {
			t.Errorf("%s: creationGroup %q authMethodLabel %v", at.ID, at.CreationGroup, at.AuthMethodLabel)
		}
		if len(at.Platforms) != 1 || at.Platforms[0].Platform != "openai" || !slices.Equal(at.Platforms[0].Endpoints, endpoints[at.ID]) {
			t.Fatalf("%s: platforms = %+v, want openai %v", at.ID, at.Platforms, endpoints[at.ID])
		}
		if !slices.Equal(at.Platforms[0].PassHeaders, fromPlatform) {
			t.Errorf("%s: passHeaders = %v, want the openai platform's without %v: %v", at.ID, at.Platforms[0].PassHeaders, notForwarded, fromPlatform)
		}
		if !slices.Equal(at.SensitiveFields, []string{"api_key"}) || len(at.GuardedSettings) != 0 || len(at.DefaultModels) != 0 {
			t.Errorf("%s: sensitiveFields %v guardedSettings %v defaultModels %v", at.ID, at.SensitiveFields, at.GuardedSettings, at.DefaultModels)
		}
		if fields, known := at.AddressFields(); !known || !slices.Equal(fields, []string{"base_url"}) || !at.CustomUpstream() {
			t.Errorf("%s: upstreamFields %v (declared %v), custom upstream %v", at.ID, fields, known, at.CustomUpstream())
		}
		if got, want := slices.Sorted(maps.Keys(types[at.ID].Routes)), endpointProtocols(at.Platforms[0].Endpoints); !slices.Equal(got, want) {
			t.Errorf("%s: routes %v, the endpoints need %v", at.ID, got, want)
		}
	}
}

// TestResourceEndpointForm is the base_url rule of both forms: the address
// of the resource and nothing else, optionally with the /openai or
// /openai/v1 suffix the types remove, or a Foundry project path. A pasted
// request URL (a deployment, an operation, an api-version) is refused with a
// message that says what to enter, instead of being saved and answering 404
// on every request. The suffixes are matched in the case the types remove
// them in (the removal is case-sensitive), so another casing is refused too
// rather than accepted and left in the address. What the form accepts is
// saved as the bare address and gives working request URLs.
func TestResourceEndpointForm(t *testing.T) {
	rejected := []string{
		"",
		resource + "/openai/deployments/gpt-4o",
		resource + "/openai/deployments/gpt-4o/chat/completions",
		resource + "/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21",
		resource + "/openai/v1/chat/completions",
		resource + "/openai/v1/responses",
		resource + "/models",
		resource + "/OpenAI/V1",
		resource + "/Openai",
		resource + "/openai/v1//",
		resource + "/openai/v1/openai",
		resource + "?api-version=2024-10-21",
		resource + "/?api-version=2024-10-21",
		resource + "/openai?api-version=2024-10-21",
		resource + "/openai/v1?x=1",
		resource + "#keys",
		resource + "/api/projects",
		resource + "/api/projects/",
		resource + "/api/projects/p/openai/v1",
		resource + "/api/Projects/p",
		"https://key@res.openai.azure.com",
		"https://",
		"HTTPS://res.openai.azure.com",
		"res.openai.azure.com",
		"ftp://res.openai.azure.com",
		" " + resource,
		resource + " ",
	}
	var patterns []string
	for _, accountType := range []string{typeV1, typeDeployment} {
		var schema struct {
			Properties map[string]struct {
				Pattern string `json:"pattern"`
			} `json:"properties"`
		}
		readJSON(t, path.Join("forms", accountType+".schema.json"), &schema)
		pattern := schema.Properties["base_url"].Pattern
		patterns = append(patterns, pattern)
		re := regexp.MustCompile(pattern) // RE2, as the core compiles it
		for _, in := range rejected {
			if re.MatchString(in) {
				t.Errorf("%s: %q is accepted", accountType, in)
			}
		}
		for in, base := range acceptedEndpoints {
			if !re.MatchString(in) {
				t.Errorf("%s: %q is refused", accountType, in)
				continue
			}
			assertEndpoint(t, accountType, in, base)
		}

		// The refusal says what to enter, in both languages.
		var ui struct {
			BaseURL struct {
				Messages map[string]map[string]string `json:"ui:errorMessages"`
			} `json:"base_url"`
		}
		readJSON(t, path.Join("forms", accountType+".ui.json"), &ui)
		msg := ui.BaseURL.Messages["pattern"]
		if !strings.Contains(msg["en"], "resource endpoint only") || msg["zh"] != "只填资源地址，不要带 deployments、chat/completions 或 api-version" {
			t.Errorf("%s: base_url pattern message = %v", accountType, msg)
		}
	}
	if patterns[0] != patterns[1] {
		t.Errorf("the two forms check base_url differently: %q, %q", patterns[0], patterns[1])
	}
}

// TestForms pins the form rules the types rely on: the classic type always
// has an api-version (required, prefilled with the GA version), and both
// take a key without a vendor prefix.
func TestForms(t *testing.T) {
	type property struct {
		Default   string `json:"default"`
		Pattern   string `json:"pattern"`
		MinLength int    `json:"minLength"`
		MaxLength int    `json:"maxLength"`
	}
	type schema struct {
		Required             []string            `json:"required"`
		AdditionalProperties bool                `json:"additionalProperties"`
		Properties           map[string]property `json:"properties"`
	}
	for accountType, required := range map[string][]string{
		typeV1:         {"base_url", "api_key"},
		typeDeployment: {"base_url", "api_key", "api_version"},
	} {
		var s schema
		readJSON(t, path.Join("forms", accountType+".schema.json"), &s)
		if !slices.Equal(s.Required, required) || s.AdditionalProperties || len(s.Properties) != len(required) {
			t.Errorf("%s: required %v, properties %v", accountType, s.Required, s.Properties)
		}
		if k := s.Properties["api_key"]; k.MinLength != 8 || k.MaxLength != 512 || !regexp.MustCompile(k.Pattern).MatchString(apiKey) {
			t.Errorf("%s: api_key %+v", accountType, k)
		}
	}
	var s schema
	readJSON(t, path.Join("forms", typeDeployment+".schema.json"), &s)
	v := s.Properties["api_version"]
	if v.Default != "2024-10-21" {
		t.Errorf("api_version default %q", v.Default)
	}
	re := regexp.MustCompile(v.Pattern)
	for value, ok := range map[string]bool{"2024-10-21": true, "2025-04-01-preview": true, "": false, "latest": false, "2024-10-21&x=1": false} {
		if re.MatchString(value) != ok {
			t.Errorf("api_version %q: match %v, want %v", value, !ok, ok)
		}
	}
}

// endpointProtocols are the protocols of the openai endpoints ids, sorted.
func endpointProtocols(ids []string) []string {
	var out []string
	for _, e := range builtinPlatform("openai").Endpoints {
		if slices.Contains(ids, e.ID) {
			out = append(out, e.Protocol)
		}
	}
	slices.Sort(out)
	return out
}

func builtinPlatform(id string) *manifest.Platform {
	for _, p := range platforms.Builtin() {
		if p.ID == id {
			return &p
		}
	}
	return nil
}

// readJSON decodes a package file into v.
func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// load decodes the embedded manifest strictly and checks it with the files
// a package carries, as the core does before it installs a package
// (tooling mode: the binaries come from the build).
func load(t *testing.T) manifest.Manifest {
	t.Helper()
	var m manifest.Manifest
	dec := json.NewDecoder(bytes.NewReader(manifestJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	files := map[string][]byte{"manifest.json": manifestJSON}
	for _, dir := range []string{"forms", "assets"} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			files[p] = raw
		}
	}
	if err := check.Validate(&m, files, check.ValidateOptions{Tooling: true}); err != nil {
		t.Fatal(err)
	}
	if w := check.Warnings(&m, files); len(w) != 0 {
		t.Fatalf("warnings: %v", w)
	}
	return m
}

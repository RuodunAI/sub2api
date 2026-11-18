package check

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// hasCode reports whether err holds code on field (codes keeps only the
// last code of a field).
func hasCode(err error, field, code string) bool {
	fields, _ := Fields(err)
	return slices.ContainsFunc(fields, func(f FieldError) bool { return f.Field == field && f.Code == code })
}

// controlFiles are the package files of controlsManifest plus manifest.json
// as written, with edit applied to its decoded form: the fields the manifest
// types no longer have can only be read from there.
func controlFiles(t *testing.T, m *manifest.Manifest, edit func(root map[string]any)) map[string][]byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	edit(root)
	if b, err = json.Marshal(root); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"forms/k.json": []byte(`{}`), "manifest.json": b}
}

// at returns the i-th object of the array root[key].
func at(root map[string]any, key string, i int) map[string]any {
	return root[key].([]any)[i].(map[string]any)
}

// Everything only a package of the contract before the single Execute path
// declares is refused with CodeOutdatedContract, so the host can answer with
// the one message for such packages.
func TestOutdatedContract(t *testing.T) {
	if err := Validate(controlsManifest(nil), controlFiles(t, controlsManifest(nil), func(map[string]any) {}), ValidateOptions{Tooling: true}); err != nil || OutdatedContract(err) {
		t.Fatalf("current manifest: %v", err)
	}
	for _, tc := range []struct {
		name  string
		typed func(m *manifest.Manifest)
		raw   func(root map[string]any)
		field string
	}{
		{name: "runtime none", field: "runtime",
			typed: func(m *manifest.Manifest) { m.Runtime, m.Entry = "none", manifest.Entry{} }},
		{name: "platform.execute.v1", field: "capabilities[1]",
			typed: func(m *manifest.Manifest) {
				m.Capabilities = append(m.Capabilities, manifest.Capability{ID: "platform.execute.v1"})
			}},
		{name: "platform.poll.v1", field: "capabilities[1]",
			typed: func(m *manifest.Manifest) {
				m.Capabilities = append(m.Capabilities, manifest.Capability{ID: "platform.poll.v1"})
			}},
		{name: "upstream block", field: "accountTypes[0].upstream",
			raw: func(root map[string]any) {
				at(root, "accountTypes", 0)["upstream"] = map[string]any{"preset": "openai"}
			}},
		{name: "platform requestFields", field: "platforms[0].requestFields",
			raw: func(root map[string]any) { at(root, "platforms", 0)["requestFields"] = []string{"model"} }},
		{name: "account platform requestFields", field: "accountTypes[0].platforms[0].requestFields",
			raw: func(root map[string]any) {
				at(at(root, "accountTypes", 0), "platforms", 0)["requestFields"] = []string{"model"}
			}},
		{name: "endpoint maxBodyBytes", field: "platforms[0].endpoints[0].request.maxBodyBytes",
			raw: func(root map[string]any) {
				at(at(root, "platforms", 0), "endpoints", 0)["request"].(map[string]any)["maxBodyBytes"] = 1 << 20
			}},
	} {
		m := controlsManifest(nil)
		if tc.typed != nil {
			tc.typed(m)
		}
		raw := tc.raw
		if raw == nil {
			raw = func(map[string]any) {}
		}
		err := Validate(m, controlFiles(t, m, raw), ValidateOptions{Tooling: true})
		if !hasCode(err, tc.field, CodeOutdatedContract) || !OutdatedContract(err) {
			t.Errorf("%s: want %s on %s, got %v", tc.name, CodeOutdatedContract, tc.field, err)
		}
	}
}

// Another runtime is unknown, not outdated.
func TestRuntimeUnsupported(t *testing.T) {
	m := controlsManifest(nil)
	m.Runtime = "wasm"
	err := Validate(m, nil, ValidateOptions{Tooling: true})
	if !hasCode(err, "runtime", "unsupported") || OutdatedContract(err) {
		t.Fatalf("errors = %v", err)
	}
}

// Every account type is served by the plugin's own Execute, which receives
// the credentials.
func TestAccountTypesNeedCredentials(t *testing.T) {
	m := controlsManifest(nil)
	m.HostPermissions = m.HostPermissions[:2]
	if got := controlCodes(m); got["accountTypes"] != "missing_host_permission" {
		t.Fatalf("codes = %v", got)
	}
}

// Usage and task facts come back from the Execute of the plugin serving the
// account: an account type cannot serve, on another plugin's platform, an
// endpoint whose facts that plugin reads.
func TestForeignFacts(t *testing.T) {
	usage, task, plain := pluginSourcePlatform().Endpoints[0], taskPlatform().Endpoints[0], validPlatform().Endpoints[0]
	usage.ID, task.ID, plain.ID = "usage", "task", "plain"
	other := []PlatformOwner{{PluginKey: "other", ID: "foreign", Endpoints: []manifest.Endpoint{usage, task, plain}}}
	for _, tc := range []struct {
		name      string
		endpoints []string
		rejected  bool
	}{
		{"all endpoints", nil, true},
		{"plugin usage", []string{"usage"}, true},
		{"task submission", []string{"task"}, true},
		{"declarative endpoint", []string{"plain"}, false},
	} {
		m := controlsManifest(nil)
		m.AccountTypes[0].Platforms = []manifest.AccountPlatform{{Platform: "foreign", Endpoints: tc.endpoints}}
		err := Validate(m, map[string][]byte{"forms/k.json": []byte(`{}`)}, ValidateOptions{Tooling: true, OtherPlatforms: other})
		if got := hasCode(err, "accountTypes[0].platforms[0].platform", "foreign_facts"); got != tc.rejected {
			t.Errorf("%s: rejected = %v, errors = %v", tc.name, got, err)
		}
	}
	// The plugin's own platform is its own to serve.
	m := controlsManifest(nil)
	m.Platforms = []manifest.Platform{pluginSourcePlatform()}
	if got := controlCodes(m); len(got) != 0 {
		t.Fatalf("own platform: %v", got)
	}
}

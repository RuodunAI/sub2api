package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/Sub2API-Devs/sup2api/next/plugins/vertex/internal/vertex"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// TestManifestPassesCoreChecks runs manifest.json through the checks the
// server runs before it installs a plugin (Tooling: the runtime binaries are
// produced by the build).
func TestManifestPassesCoreChecks(t *testing.T) {
	var m manifest.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	fields, _ := check.Fields(check.Validate(&m, packageFiles(t), check.ValidateOptions{Tooling: true}))
	for _, f := range fields {
		t.Errorf("%s: %s (%s)", f.Field, f.Message, f.Code)
	}
}

// wantType is what the manifest must declare of an account type
// (docs/DESIGN-2026-11-17-CLOUD-PROVIDERS.md §7).
type wantType struct {
	id, form, authEN, authZH string
	sensitive                []string
	refresh                  bool
	// credentialFields are the schema properties besides project_id and
	// location; hidden are those the form does not show.
	credentialFields, hidden []string
	required                 string
}

var tokenFields = []string{"access_token", "expires_at", "token_for"}

var wantTypes = []wantType{
	{vertex.TypeServiceAccount, "sa", "Service Account", "服务账号", []string{"service_account_json", "access_token"}, true,
		append([]string{"service_account_json"}, tokenFields...), tokenFields, "service_account_json"},
	{vertex.TypeAuthorizedUser, "authorized_user", "User Credentials", "用户凭证", []string{"authorized_user_json", "access_token"}, true,
		append([]string{"authorized_user_json"}, tokenFields...), tokenFields, "authorized_user_json"},
	{vertex.TypeAPIKey, "apikey", "API Key", "API Key", []string{"api_key"}, false, []string{"api_key"}, nil, "api_key"},
}

// TestManifestShape pins manifest.json and the forms to the design and to
// the constants the code uses.
func TestManifestShape(t *testing.T) {
	dec := json.NewDecoder(bytes.NewReader(manifestJSON))
	dec.DisallowUnknownFields()
	var m manifest.Manifest
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	if m.Key != "vertex" || m.Version != vertex.Version || m.Runtime != "grpc" || m.APIVersion != manifest.APIVersion || m.Publisher != "sub2api" {
		t.Fatalf("key/version/runtime = %s %s %s (plugin version %s)", m.Key, m.Version, m.Runtime, vertex.Version)
	}
	if m.Entry.GRPC == nil || m.Entry.GRPC.Binaries == "" || m.HostCompat != ">=0.1.304 <0.2.0" {
		t.Fatalf("entry/hostCompat = %+v %s", m.Entry, m.HostCompat)
	}
	if m.Name["en"] == "" || m.Name["zh"] == "" || m.Description["en"] == "" || m.Description["zh"] == "" {
		t.Fatal("name and description need en and zh")
	}
	// An ordinary forwarding plugin: no platform, routes, jobs, UI or
	// database of its own.
	if len(m.Capabilities) != 1 || m.Capabilities[0].ID != manifest.CapPlatformAdapter {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	if len(m.Platforms) != 0 || len(m.Routes) != 0 || m.UI != nil || m.Database != nil || len(m.UserPermissions) != 0 {
		t.Fatalf("the manifest declares more than account types: %+v", m)
	}

	if len(m.AccountTypes) != len(wantTypes) {
		t.Fatalf("accountTypes = %d", len(m.AccountTypes))
	}
	files := packageFiles(t)
	for i, want := range wantTypes {
		at := m.AccountTypes[i]
		if at.ID != want.id {
			t.Fatalf("accountTypes[%d] = %s, want %s", i, at.ID, want.id)
		}
		if at.Form.Mode != "schema" || at.Form.Schema != "forms/"+want.form+".schema.json" || at.Form.UISchema != "forms/"+want.form+".ui.json" {
			t.Errorf("%s form = %+v", at.ID, at.Form)
		}
		// One entry in the "new account" dialog, switched by auth method.
		if at.CreationGroup != "vertex" || at.AuthMethodLabel["en"] != want.authEN || at.AuthMethodLabel["zh"] != want.authZH {
			t.Errorf("%s creationGroup/authMethodLabel = %q %v", at.ID, at.CreationGroup, at.AuthMethodLabel)
		}
		if at.Label["en"] == "" || at.Label["zh"] == "" || at.Description["en"] == "" || at.Description["zh"] == "" {
			t.Errorf("%s needs en and zh label and description", at.ID)
		}
		// The host comes from the location: no address field, nothing guarded.
		if at.UpstreamFields == nil || len(at.UpstreamFields) != 0 || len(at.GuardedSettings) != 0 {
			t.Errorf("%s upstreamFields/guardedSettings = %v %v", at.ID, at.UpstreamFields, at.GuardedSettings)
		}
		if !slices.Equal(at.SensitiveFields, want.sensitive) || !slices.Equal(at.SettingsFields, []string{"project_id", "location"}) {
			t.Errorf("%s sensitive/settings = %v %v", at.ID, at.SensitiveFields, at.SettingsFields)
		}
		// Default refresh declaration: expires_at, 30 minutes ahead.
		if (at.Refresh != nil) != want.refresh || (at.Refresh != nil && (at.Refresh.Field() != "expires_at" || at.Refresh.BeforeExpirySec != 0)) {
			t.Errorf("%s refresh = %+v", at.ID, at.Refresh)
		}
		if len(at.Platforms) != 1 || at.Platforms[0].Platform != vertex.PlatformID ||
			!slices.Equal(at.Platforms[0].Endpoints, []string{"generate_content", "stream_generate_content", "count_tokens"}) {
			t.Errorf("%s platforms = %+v", at.ID, at.Platforms)
		}
		if !slices.Contains(at.DefaultModels, vertex.DefaultTestModel) || len(at.DefaultModelMapping) != 0 {
			t.Errorf("%s defaultModels must hold the test model and no mapping: %v %v", at.ID, at.DefaultModels, at.DefaultModelMapping)
		}
		for _, model := range at.DefaultModels {
			if !strings.HasPrefix(model, "gemini-") {
				t.Errorf("%s default model %q is not a Gemini publisher model", at.ID, model)
			}
		}
		if !slices.Equal(at.DefaultModels, m.AccountTypes[0].DefaultModels) {
			t.Errorf("%s default models differ from %s", at.ID, m.AccountTypes[0].ID)
		}
		checkForm(t, want, files[at.Form.Schema], files[at.Form.UISchema])
	}

	perms := map[string]manifest.HostPermission{}
	for _, perm := range m.HostPermissions {
		if perm.Reason["en"] == "" || perm.Reason["zh"] == "" {
			t.Errorf("host permission %s needs an en and zh reason", perm.ID)
		}
		perms[perm.ID] = perm
	}
	// No "net": the core sends the upstream and token requests.
	if len(perms) != 2 || perms["platform.register"].ID == "" || perms["accounts.credentials"].Scope["types"] != "own" {
		t.Errorf("host permissions = %v", perms)
	}
	// No externalServices: they are the hosts the plugin's own Fetch and
	// SendAccountRequest may reach, and it makes neither call. The core
	// sends the upstream, token, test and model requests the plugin builds;
	// none of those is checked against externalServices.
	if len(m.ExternalServices) != 0 {
		t.Errorf("externalServices = %v, want none", m.ExternalServices)
	}
}

// checkForm pins a form: a closed schema whose properties are exactly the
// fields the plugin reads, with the token fields present but hidden (the
// editor sends the whole credential object back, so they must be allowed).
func checkForm(t *testing.T, want wantType, schema, ui []byte) {
	t.Helper()
	if len(schema) == 0 || len(ui) == 0 {
		t.Fatalf("%s: form files missing", want.id)
	}
	s := gjson.ParseBytes(schema)
	if s.Get("additionalProperties").Type != gjson.False || compact(s.Get("required").Raw) != `["`+want.required+`"]` {
		t.Errorf("%s schema additionalProperties/required = %s %s", want.id, s.Get("additionalProperties").Raw, s.Get("required").Raw)
	}
	var props []string
	s.Get("properties").ForEach(func(k, _ gjson.Result) bool { props = append(props, k.String()); return true })
	slices.Sort(props)
	wantProps := append(slices.Clone(want.credentialFields), "location", "project_id")
	slices.Sort(wantProps)
	if !slices.Equal(props, wantProps) {
		t.Errorf("%s schema properties = %v, want %v", want.id, props, wantProps)
	}
	// The patterns are the plugin's own, with the empty string allowed.
	if s.Get("properties.project_id.pattern").String() != `^([a-z][a-z0-9-]{4,28}[a-z0-9])?$` ||
		s.Get("properties.location.pattern").String() != `^([a-z0-9-]{2,32})?$` {
		t.Errorf("%s project/location patterns = %s %s", want.id, s.Get("properties.project_id.pattern"), s.Get("properties.location.pattern"))
	}
	wantDefault := ""
	if want.refresh {
		wantDefault = vertex.LocationGlobal
	}
	if s.Get("properties.location.default").String() != wantDefault {
		t.Errorf("%s location default = %q, want %q", want.id, s.Get("properties.location.default").String(), wantDefault)
	}
	if want.refresh && s.Get("properties.expires_at.type").String() != "integer" {
		t.Errorf("%s expires_at must be an integer", want.id)
	}
	u := gjson.ParseBytes(ui)
	if u.Get(want.required+".ui:widget").String() != "secret" {
		t.Errorf("%s: %s must be a secret", want.id, want.required)
	}
	for _, p := range props {
		isHidden := u.Get(p+".ui:widget").String() == "hidden"
		if isHidden != slices.Contains(want.hidden, p) {
			t.Errorf("%s: %s hidden = %v", want.id, p, isHidden)
		}
		if isHidden {
			continue
		}
		if title := u.Get(p + ".ui:title"); title.Get("en").String() == "" || title.Get("zh").String() == "" {
			t.Errorf("%s: %s needs an en and zh title", want.id, p)
		}
	}
}

func compact(raw string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(raw)) != nil {
		return raw
	}
	return buf.String()
}

// TestManifestServes runs the plugin with its embedded manifest.
func TestManifestServes(t *testing.T) {
	h := pluginsdktest.Start(t, vertex.New(), pluginsdktest.Options{Manifest: manifestJSON})
	if h.Info.GetPluginKey() != "vertex" || h.Info.GetVersion() != vertex.Version {
		t.Fatalf("info = %v", h.Info)
	}
	if !slices.Contains(h.Info.GetCapabilities(), manifest.CapPlatformAdapter) {
		t.Errorf("capabilities = %v", h.Info.GetCapabilities())
	}
}

// packageFiles collects the files a package would carry.
func packageFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == ".":
			return nil
		case strings.HasPrefix(d.Name(), "."):
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case d.IsDir() || !d.Type().IsRegular() || strings.HasSuffix(p, ".go"):
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(p)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

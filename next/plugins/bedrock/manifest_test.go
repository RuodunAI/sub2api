package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/Sub2API-Devs/sup2api/next/plugins/bedrock/internal/bedrock"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// hostCompat is the range of cores the plugin installs on. Its lower bound
// is the first core release that signs a built request and decodes the
// event stream (BuildUpstreamRequestResponse.signing / response_stream): an
// older core ignores both fields and would send access key requests
// unsigned.
const hostCompat = ">=0.1.304 <0.2.0"

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

// The plugin refuses cores without the signing fields.
func TestHostCompat(t *testing.T) {
	for version, want := range map[string]bool{
		"0.1.290": false, "0.1.302": false, "0.1.303": false,
		"0.1.304": true, "0.1.304-rc.1": true, "0.1.999": true,
		"0.2.0": false,
	} {
		if got, err := check.HostCompatible(hostCompat, version); err != nil || got != want {
			t.Errorf("core %s: compatible = %v (%v), want %v", version, got, err, want)
		}
	}
}

// wantType is what the manifest must declare of an account type
// (docs/DESIGN-2026-11-17-CLOUD-PROVIDERS.md §9).
type wantType struct {
	id, form, authEN, authZH string
	sensitive                []string
	settings                 []string
	// credentials are the schema properties besides the shared settings, in
	// form order; all of them are required but session_expires_at.
	credentials []string
	// secrets are the properties shown with the secret widget.
	secrets []string
}

var sharedSettings = []string{"region", "inference_profile_prefix"}

var wantTypes = []wantType{
	{bedrock.TypeAPIKey, "apikey", "API Key", "API Key", []string{"api_key"}, sharedSettings,
		[]string{"api_key"}, []string{"api_key"}},
	{bedrock.TypeAccessKey, "aksk", "Access Key", "Access Key", []string{"secret_access_key"}, sharedSettings,
		[]string{"access_key_id", "secret_access_key"}, []string{"secret_access_key"}},
	{bedrock.TypeSTS, "sts", "Temporary Credentials", "临时凭证", []string{"secret_access_key", "session_token"}, append(slices.Clone(sharedSettings), "session_expires_at"),
		[]string{"access_key_id", "secret_access_key", "session_token", "session_expires_at"}, []string{"secret_access_key", "session_token"}},
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
	if m.Key != "bedrock" || !regexp.MustCompile(`^[a-z][a-z0-9_]{1,29}$`).MatchString(m.Key) ||
		m.Version != bedrock.Version || m.Runtime != "grpc" || m.APIVersion != manifest.APIVersion || m.Publisher != "sub2api" {
		t.Fatalf("key/version/runtime = %s %s %s (plugin version %s)", m.Key, m.Version, m.Runtime, bedrock.Version)
	}
	if m.Entry.GRPC == nil || m.Entry.GRPC.Binaries == "" || m.HostCompat != hostCompat {
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
	if _, err := os.Stat(m.Icon); err != nil || m.Icon != "assets/bedrock-color.svg" {
		t.Fatalf("icon %q: %v", m.Icon, err)
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
		if at.CreationGroup != "bedrock" || at.AuthMethodLabel["en"] != want.authEN || at.AuthMethodLabel["zh"] != want.authZH {
			t.Errorf("%s creationGroup/authMethodLabel = %q %v", at.ID, at.CreationGroup, at.AuthMethodLabel)
		}
		if at.Label["en"] == "" || at.Label["zh"] == "" || at.Description["en"] == "" || at.Description["zh"] == "" {
			t.Errorf("%s needs en and zh label and description", at.ID)
		}
		// The host comes from the region: no address field, nothing guarded.
		if at.UpstreamFields == nil || len(at.UpstreamFields) != 0 || len(at.GuardedSettings) != 0 || at.Upstream != nil {
			t.Errorf("%s upstreamFields/guardedSettings/upstream = %v %v %v", at.ID, at.UpstreamFields, at.GuardedSettings, at.Upstream)
		}
		if !slices.Equal(at.SensitiveFields, want.sensitive) || !slices.Equal(at.SettingsFields, want.settings) {
			t.Errorf("%s sensitive/settings = %v %v", at.ID, at.SensitiveFields, at.SettingsFields)
		}
		// Nothing renews the credentials, no quota, no balance, and the
		// answer is metered by the platforms' own rules.
		if at.Refresh != nil || at.Quota != nil || at.Balance != nil {
			t.Errorf("%s declares refresh, quota or balance", at.ID)
		}
		checkPlatforms(t, at)
		checkModels(t, at, m.AccountTypes[0])
		checkForm(t, want, files[at.Form.Schema], files[at.Form.UISchema])
		// The parts of the form every type shares are the same text.
		for _, field := range sharedSettings {
			first := gjson.GetBytes(files[m.AccountTypes[0].Form.UISchema], field).Raw
			if got := gjson.GetBytes(files[at.Form.UISchema], field).Raw; compact(got) != compact(first) {
				t.Errorf("%s: the form's %s differs from %s's", at.ID, field, m.AccountTypes[0].ID)
			}
			if schemaProperty(files[at.Form.Schema], field) != schemaProperty(files[m.AccountTypes[0].Form.Schema], field) {
				t.Errorf("%s: the schema's %s differs from %s's", at.ID, field, m.AccountTypes[0].ID)
			}
		}
	}

	perms := map[string]manifest.HostPermission{}
	for _, perm := range m.HostPermissions {
		if perm.Reason["en"] == "" || perm.Reason["zh"] == "" {
			t.Errorf("host permission %s needs an en and zh reason", perm.ID)
		}
		perms[perm.ID] = perm
	}
	// No "net": the core signs and sends the upstream requests.
	if len(perms) != 2 || perms["platform.register"].ID == "" || perms["accounts.credentials"].Scope["types"] != "own" {
		t.Errorf("host permissions = %v", perms)
	}
	// The core takes an entry as a host or the domain above it, never as a
	// pattern: the one entry is the domain every region's host is under.
	if !slices.Equal(m.ExternalServices, []string{"amazonaws.com"}) || bedrock.ServiceDomain != "amazonaws.com" {
		t.Errorf("externalServices = %v", m.ExternalServices)
	}
	for _, region := range []string{bedrock.DefaultRegion, "eu-central-1", "us-gov-west-1"} {
		host := bedrock.RuntimeHost(region)
		if host != "bedrock-runtime."+region+".amazonaws.com" || !strings.HasSuffix(host, "."+m.ExternalServices[0]) {
			t.Errorf("%s: host %s is not under %v", region, host, m.ExternalServices)
		}
	}
}

// checkPlatforms pins what a type serves: Claude's messages endpoint only.
// The model, the body's anthropic_beta and the beta header are all the
// plugin reads of a request. No type declares the openai platform: a native
// openai route would take the place of the core's conversion, and a client
// speaking an OpenAI protocol would be sent to Bedrock's /openai/v1 instead
// of InvokeModel. count_tokens is left to the core.
func checkPlatforms(t *testing.T, at manifest.AccountType) {
	t.Helper()
	if len(at.Platforms) != 1 {
		t.Fatalf("%s platforms = %+v", at.ID, at.Platforms)
	}
	claude := at.Platforms[0]
	if claude.Platform != "anthropic" || !slices.Equal(claude.Endpoints, []string{"messages"}) ||
		!slices.Equal(claude.RequestFields, []string{"model", "anthropic_beta"}) || !slices.Equal(claude.PassHeaders, []string{"anthropic-beta"}) {
		t.Errorf("%s anthropic platform = %+v", at.ID, claude)
	}
	if len(claude.Usage) != 0 || len(claude.StreamOnly) != 0 {
		t.Errorf("%s overrides usage or streamOnly", at.ID)
	}
}

// defaultModelMapping is the mapping every type prefills: Anthropic's model
// names to Bedrock model ids without a geography prefix (the account's
// prefix setting adds it). The ids are those of the AWS model cards
// (https://docs.aws.amazon.com/bedrock/latest/userguide/model-cards-anthropic.html):
// models up to Opus 4.6 carry a version suffix, the later ones do not.
var defaultModelMapping = map[string]string{
	"claude-fable-5-1":           "anthropic.claude-fable-5-1",
	"claude-fable-5":             "anthropic.claude-fable-5",
	"claude-opus-5":              "anthropic.claude-opus-5",
	"claude-opus-4-8":            "anthropic.claude-opus-4-8",
	"claude-opus-4-7":            "anthropic.claude-opus-4-7",
	"claude-opus-4-6":            "anthropic.claude-opus-4-6-v1",
	"claude-opus-4-5":            "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-5-20251101":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-1":            "anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-opus-4-20250514":     "anthropic.claude-opus-4-20250514-v1:0",
	"claude-sonnet-5":            "anthropic.claude-sonnet-5",
	"claude-sonnet-4-6":          "anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-5":          "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-20250929": "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-20250514":   "anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-haiku-4-5":           "anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-haiku-4-5-20251001":  "anthropic.claude-haiku-4-5-20251001-v1:0",
}

// checkModels pins the model defaults: the three types prefill the same
// models and the same mapping, clients name Claude models the Anthropic way
// and every one of them is mapped.
func checkModels(t *testing.T, at, first manifest.AccountType) {
	t.Helper()
	if !slices.Equal(at.DefaultModels, first.DefaultModels) || !maps.Equal(at.DefaultModelMapping, first.DefaultModelMapping) {
		t.Errorf("%s model defaults differ from %s's", at.ID, first.ID)
	}
	if !maps.Equal(at.DefaultModelMapping, defaultModelMapping) {
		t.Errorf("%s defaultModelMapping = %v", at.ID, at.DefaultModelMapping)
	}
	if !slices.Equal(slices.Sorted(slices.Values(at.DefaultModels)), slices.Sorted(maps.Keys(defaultModelMapping))) {
		t.Errorf("%s defaultModels = %v, the mapping has %d", at.ID, at.DefaultModels, len(defaultModelMapping))
	}
	targets := map[string]bool{}
	for _, model := range at.DefaultModels {
		id := at.DefaultModelMapping[model]
		// A dated name and its alias lead to the same Bedrock model.
		if !strings.HasPrefix(model, "claude-") || !strings.HasPrefix(id, "anthropic."+model) {
			t.Errorf("%s: default model %q maps to %q", at.ID, model, id)
		}
		targets[id] = true
	}
	if !targets[bedrock.DefaultTestModel] {
		t.Errorf("%s: the default test model %s is no target of the default mapping", at.ID, bedrock.DefaultTestModel)
	}
}

// checkForm pins a form: a closed schema whose properties are exactly the
// fields the plugin reads, bounded the way the plugin validates them.
func checkForm(t *testing.T, want wantType, schema, ui []byte) {
	t.Helper()
	if len(schema) == 0 || len(ui) == 0 {
		t.Fatalf("%s: form files missing", want.id)
	}
	s, u := gjson.ParseBytes(schema), gjson.ParseBytes(ui)
	required := slices.DeleteFunc(slices.Clone(want.credentials), func(f string) bool { return f == "session_expires_at" })
	if s.Get("additionalProperties").Type != gjson.False || !slices.Equal(stringsOf(s.Get("required")), required) {
		t.Errorf("%s schema additionalProperties/required = %s %s", want.id, s.Get("additionalProperties").Raw, s.Get("required").Raw)
	}
	fields := append(slices.Clone(want.credentials), sharedSettings...)
	var props []string
	s.Get("properties").ForEach(func(k, _ gjson.Result) bool { props = append(props, k.String()); return true })
	if !slices.Equal(slices.Sorted(slices.Values(props)), slices.Sorted(slices.Values(fields))) {
		t.Errorf("%s schema properties = %v, want %v", want.id, props, fields)
	}
	// The form shows the credentials first, then the shared settings.
	if !slices.Equal(stringsOf(u.Get("ui:order")), fields) {
		t.Errorf("%s ui:order = %s", want.id, u.Get("ui:order").Raw)
	}

	// The region goes into the host name: the plugin's own strict pattern,
	// with the empty string allowed (it means the default region).
	region := s.Get("properties.region")
	if region.Get("pattern").String() != `^([a-z]{2}(-[a-z]+)+-\d)?$` || region.Get("default").String() != bedrock.DefaultRegion {
		t.Errorf("%s region = %s", want.id, region.Raw)
	}
	prefix := s.Get("properties.inference_profile_prefix")
	if !slices.Equal(stringsOf(prefix.Get("enum")), bedrock.ProfilePrefixes) || prefix.Get("default").String() != bedrock.NoProfilePrefix {
		t.Errorf("%s inference_profile_prefix = %s", want.id, prefix.Raw)
	}
	if !slices.Contains(bedrock.ProfilePrefixes, "in") {
		t.Errorf("the India geography (in.) is not a prefix: %v", bedrock.ProfilePrefixes)
	}
	if u.Get("inference_profile_prefix.ui:widget").String() != "select" || len(u.Get("inference_profile_prefix.ui:enumNames").Array()) != len(bedrock.ProfilePrefixes) {
		t.Errorf("%s: the prefix is a select with a name per value", want.id)
	}
	// The known limitations are stated where the administrator reads them:
	// nothing deep in the body is cleaned, no beta feature is added for
	// what the body holds, and the China regions are not served.
	for _, lang := range []string{"en", "zh"} {
		note := u.Get("region.ui:section.description." + lang).String()
		for _, part := range []string{"Claude Code", "cache_control", "computer use", "tool search", "output_format", "metadata"} {
			if !strings.Contains(note, part) {
				t.Errorf("%s: the %s form does not mention %s among the limitations", want.id, lang, part)
			}
		}
		for _, part := range []string{"cn-*", "us-iso-*"} {
			if help := u.Get("region.ui:help." + lang).String(); !strings.Contains(help, part) {
				t.Errorf("%s: the %s region help does not say the %s regions are not supported", want.id, lang, part)
			}
		}
	}

	for _, p := range fields {
		if (u.Get(p+".ui:widget").String() == "secret") != slices.Contains(want.secrets, p) {
			t.Errorf("%s: %s secret widget = %q", want.id, p, u.Get(p+".ui:widget").String())
		}
		if title := u.Get(p + ".ui:title"); title.Get("en").String() == "" || title.Get("zh").String() == "" {
			t.Errorf("%s: %s needs an en and zh title", want.id, p)
		}
		if help := u.Get(p + ".ui:help"); help.Get("en").String() == "" || help.Get("zh").String() == "" {
			t.Errorf("%s: %s needs an en and zh help", want.id, p)
		}
	}
	if slices.Contains(want.credentials, "access_key_id") && s.Get("properties.access_key_id.pattern").String() != `^[A-Z0-9]{16,128}$` {
		t.Errorf("%s access_key_id pattern = %s", want.id, s.Get("properties.access_key_id.pattern"))
	}
	if slices.Contains(want.credentials, "api_key") && (s.Get("properties.api_key.minLength").Int() != 8 || s.Get("properties.api_key.maxLength").Int() != 2048) {
		t.Errorf("%s api_key bounds = %s", want.id, s.Get("properties.api_key").Raw)
	}
	if !slices.Contains(want.credentials, "session_expires_at") {
		return
	}
	// The expiry of temporary credentials is a hint, and the form says that
	// nothing renews them.
	expiry := u.Get("session_expires_at")
	if s.Get("properties.session_expires_at.format").String() != "date-time" || expiry.Get("ui:widget").String() != "date-time" ||
		!strings.Contains(expiry.Get("ui:help.en").String(), "not renewed") || !strings.Contains(expiry.Get("ui:help.zh").String(), "不会自动续期") {
		t.Errorf("%s session_expires_at = %s / %s", want.id, s.Get("properties.session_expires_at").Raw, expiry.Raw)
	}
}

// stringsOf are the strings of a JSON array.
func stringsOf(r gjson.Result) []string {
	out := []string{}
	for _, v := range r.Array() {
		out = append(out, v.String())
	}
	return out
}

// schemaProperty is a schema property as its compact JSON.
func schemaProperty(schema []byte, name string) string {
	return compact(gjson.GetBytes(schema, "properties."+name).Raw)
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
	h := pluginsdktest.Start(t, bedrock.New(), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithManifest(manifestJSON)}})
	if h.Info.GetPluginKey() != "bedrock" || h.Info.GetVersion() != bedrock.Version {
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

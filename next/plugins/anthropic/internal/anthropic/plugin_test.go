package anthropic

import (
	"slices"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

func start(t *testing.T) *pluginsdktest.Harness {
	t.Helper()
	return pluginsdktest.Start(t, New(), pluginsdktest.Options{SDK: []pluginsdk.Option{pluginsdk.WithInfo("anthropic", "0.4.0")}})
}

// TestCapabilities: one process serves the apikey platform and the model
// catalog route.
func TestCapabilities(t *testing.T) {
	got := start(t).Info.GetCapabilities()
	slices.Sort(got)
	if want := []string{manifest.CapHTTPRoutes, manifest.CapPlatformAdapter}; !slices.Equal(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

// TestGolden replays the frozen golden cases of the anthropic plugin
// against the platform the plugin serves, which now refuses a key the
// account form never sends.
func TestGolden(t *testing.T) {
	standardtest.Run(t, "anthropic", "../../manifest.json", New().Platform,
		goldentest.Except("validate valid with other fields", "the account form's keys only: the others are refused"))
}

// TestAPIKey: the type passes the library's checks and keeps what the
// released plugin declared: the anthropic preset with no filter fields.
func TestAPIKey(t *testing.T) {
	at := APIKey()
	if err := standard.Check(at); err != nil {
		t.Fatal(err)
	}
	if at.ID != "apikey" || len(at.FilterFields) != 0 {
		t.Fatalf("type %q filters %v, want apikey filtering nothing", at.ID, at.FilterFields)
	}
}

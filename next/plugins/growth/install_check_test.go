package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest/check"
)

// TestManifestPassesCoreChecks runs manifest.json through the checks the
// server runs before installing a plugin (sdk/manifest/check, tooling mode).
func TestManifestPassesCoreChecks(t *testing.T) {
	raw, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	files := map[string][]byte{}
	err = filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".exe") {
			return nil
		}
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(p)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := check.Fields(check.Validate(&m, files, check.ValidateOptions{Tooling: true}))
	for _, f := range fields {
		t.Errorf("%s: %s (%s)", f.Field, f.Message, f.Code)
	}
}

// TestSettingsForm: the settings are the native GrowthSettings form; the
// JSON Schema form it replaced marked no secret, so ui.settings.secrets
// masks nothing.
func TestSettingsForm(t *testing.T) {
	rawManifest, err := os.ReadFile("manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	// "secrets" is written out: without the key the core masks every
	// setting (it cannot tell which are secrets).
	var declared struct {
		UI struct {
			Settings map[string]json.RawMessage `json:"settings"`
		} `json:"ui"`
	}
	if err := json.Unmarshal(rawManifest, &declared); err != nil || declared.UI.Settings["secrets"] == nil {
		t.Fatalf("ui.settings.secrets must be written out: %v", err)
	}
	if st := m.UI.Settings; st == nil || st.Mode != "native" || st.Component != "GrowthSettings" || st.Page != "" || len(st.SecretFields()) != 0 {
		t.Fatalf("ui.settings = %+v", st)
	}
}

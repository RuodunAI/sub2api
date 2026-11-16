package updater

import (
	"maps"
	"strings"
	"testing"
)

func TestAuditActions(t *testing.T) {
	for _, tc := range []struct{ method, path, action string }{
		{"POST", "/api/v1/system/upgrades", "system.upgrade.create"},
		{"POST", "/api/v1/system/upgrades/:id/rollback", "system.upgrade.rollback"},
		{"POST", "/api/v1/system/nodes/:id/disable", "system.node.disable"},
		{"POST", "/api/v1/system/upgrades/preflight", ""},
		{"GET", "/api/v1/system/upgrades", ""},
		{"PUT", "/api/v1/system/offload", "system.offload.update"},
		{"PUT", "/api/v1/system/update-source", "system.update_source.update"},
		{"POST", "/api/v1/system/updates/apply", "system.update.apply"},
		{"GET", "/api/v1/system/update-check", ""},
		{"POST", "/api/v1/system/releases/import", ""},
	} {
		if rule := auditRules[tc.method+" "+tc.path]; rule.action != tc.action {
			t.Fatalf("%s %s: %s", tc.method, tc.path, rule.action)
		}
	}
}

// The update-source audit names what happened to the secret, never the secret.
func TestUpdateSourceAuditDetails(t *testing.T) {
	stored := `{"data":{"enabled":true,"kind":"github","repository":"owner/repo","url":"","has_secret":true}}`
	for _, tc := range []struct{ request, secret string }{
		{`{"kind":"github","repository":"owner/repo"}`, "unchanged"},
		{`{"kind":"github","repository":"owner/repo","secret":""}`, "cleared"},
		{`{"kind":"github","repository":"owner/repo","secret":"github_pat_top-secret"}`, "set"},
		{`not json`, "unchanged"},
	} {
		id, details, changed := updateSourceDetails([]byte(tc.request), []byte(stored))
		want := map[string]any{"kind": "github", "repository": "owner/repo", "url": "", "secret": tc.secret}
		if id != "" || !changed || !maps.Equal(details, want) {
			t.Fatalf("%s: %q %v %v", tc.request, id, details, changed)
		}
		for _, v := range details {
			if strings.Contains(v.(string), "top-secret") {
				t.Fatalf("secret in audit details: %v", details)
			}
		}
	}
}

// Only a started plan is an applied update.
func TestApplyAuditDetails(t *testing.T) {
	request := []byte(`{"line":"core","version":"0.1.291","confirm":true}`)
	id, details, changed := applyDetails(request, []byte(`{"data":{"status":"started","plan_id":"plan-1"}}`))
	if want := map[string]any{"line": "core", "version": "0.1.291", "plan_id": "plan-1"}; id != "plan-1" || !changed || !maps.Equal(details, want) {
		t.Fatalf("started: %q %v %v", id, details, changed)
	}
	if _, _, changed := applyDetails(request, []byte(`{"data":{"status":"needs_confirmation","reasons":["single node"]}}`)); changed {
		t.Fatal("needs_confirmation audited")
	}
}

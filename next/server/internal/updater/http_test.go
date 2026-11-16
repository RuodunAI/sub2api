package updater

import (
	"maps"
	"strings"
	"testing"
	"time"
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

// The update-source audit records what the gateway did to the secret (a
// moved source drops it although the request said nothing about it), never
// the secret, and nothing for an answer that changed nothing.
func TestUpdateSourceAuditDetails(t *testing.T) {
	request := []byte(`{"kind":"github","repository":"owner/other","secret":"github_pat_top-secret"}`)
	for _, secret := range []string{"set", "cleared", "unchanged"} {
		stored := `{"data":{"enabled":true,"kind":"github","repository":"owner/repo","url":"","has_secret":true,"changed":true,"secret_change":"` + secret + `"}}`
		id, details, changed := updateSourceDetails(request, []byte(stored))
		want := map[string]any{"kind": "github", "repository": "owner/repo", "url": "", "secret": secret}
		if id != "" || !changed || !maps.Equal(details, want) {
			t.Fatalf("%s: %q %v %v", secret, id, details, changed)
		}
		for _, v := range details {
			if strings.Contains(v.(string), "top-secret") {
				t.Fatalf("secret in audit details: %v", details)
			}
		}
	}
	unchanged := `{"data":{"kind":"github","repository":"owner/repo","changed":false,"secret_change":"unchanged"}}`
	if _, _, changed := updateSourceDetails(request, []byte(unchanged)); changed {
		t.Fatal("an update that changed nothing was audited")
	}
}

// The routes that read the update source before answering wait longer than
// the gateway's own bound on them.
func TestSlowRoutes(t *testing.T) {
	for key := range slowRoutes {
		if _, ok := map[string]bool{"GET /api/v1/system/update-check": true, "POST /api/v1/system/updates/apply": true}[key]; !ok {
			t.Fatalf("unexpected slow route %s", key)
		}
	}
	if len(slowRoutes) != 2 || slowBridgeTimeout <= 3*time.Minute || bridgeTimeout >= slowBridgeTimeout {
		t.Fatal(slowRoutes, bridgeTimeout, slowBridgeTimeout)
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

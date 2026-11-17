package codex

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// deviceQuota is the codex_device quota declaration of the manifest.
func deviceQuota(t *testing.T) *manifest.AccountQuota {
	t.Helper()
	var m manifest.Manifest
	mustJSON(t, testManifest, &m)
	for _, at := range m.AccountTypes {
		if at.ID == TypeDevice && at.Quota != nil {
			return at.Quota
		}
	}
	t.Fatal("no codex_device quota in manifest.json")
	return nil
}

// slotHeaders are x-codex-* headers putting a window of minutes in slot
// ("primary" or "secondary").
func slotHeaders(h map[string]string, slot, minutes, used, resetAt string) map[string]string {
	p := "x-codex-" + slot + "-"
	for k, v := range map[string]string{p + "window-minutes": minutes, p + "used-percent": used, p + "reset-at": resetAt} {
		if v != "" {
			h[k] = v
		}
	}
	return h
}

// The manifest's declarations, as the core reads them on every response,
// name the windows by their length whichever slot carries them.
func TestManifestQuotaHeaders(t *testing.T) {
	decls := deviceQuota(t).Headers
	for _, order := range [][2]string{{"primary", "secondary"}, {"secondary", "primary"}} {
		h := slotHeaders(map[string]string{}, order[0], "300", "42.5", "1800003600")
		slotHeaders(h, order[1], "10080", "7", "1800500000")
		got := manifest.ReadQuotaHeaders(decls, func(name string) string { return h[name] }, testNow)
		want := map[string]float64{"5h": 42.5, "7d": 7}
		if len(got) != 2 {
			t.Fatalf("%v: windows = %+v", order, got)
		}
		for _, w := range got {
			if w.Utilization != want[w.Key] || w.ResetsAt == nil {
				t.Errorf("%v: window %+v, want %v", order, w, want)
			}
		}
	}
}

func TestUsedUpUntil(t *testing.T) {
	p := newTestPlugin(DefaultEndpoints)
	future, later, past := "1800003600", "1800500000", "1799990000"
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    int64 // 0 = not used up
	}{
		{"5h full", slotHeaders(slotHeaders(map[string]string{}, "primary", "300", "100", future), "secondary", "10080", "30", later), 1800003600},
		{"weekly full in the primary slot", slotHeaders(slotHeaders(map[string]string{}, "primary", "10080", "100.0", later), "secondary", "300", "12", future), 1800500000},
		{"both full: the latest reset", slotHeaders(slotHeaders(map[string]string{}, "primary", "300", "100", future), "secondary", "10080", "101", later), 1800500000},
		{"another length counts", slotHeaders(map[string]string{}, "primary", "1440", "100", future), 1800003600},
		{"full without reset", slotHeaders(map[string]string{}, "primary", "300", "100", ""), 0},
		{"full with a past reset", slotHeaders(map[string]string{}, "primary", "300", "100", past), 0},
		{"below 100", slotHeaders(map[string]string{}, "primary", "300", "99.9", future), 0},
		{"no window length", slotHeaders(map[string]string{}, "primary", "", "100", future), 0},
		{"no headers", map[string]string{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until, ok := p.usedUpUntil(&pluginv1.ClassifyErrorRequest{Status: http.StatusTooManyRequests, Headers: tc.headers}, testNow)
			if ok != (tc.want != 0) || (ok && until.Unix() != tc.want) {
				t.Fatalf("usedUpUntil = %s %v, want %s", until, ok, time.Unix(tc.want, 0).UTC())
			}
		})
	}
	// Only a 429 counts.
	full := slotHeaders(map[string]string{}, "primary", "300", "100", future)
	if _, ok := p.usedUpUntil(&pluginv1.ClassifyErrorRequest{Status: http.StatusOK, Headers: full}, testNow); ok {
		t.Fatal("a 200 showed a used-up window")
	}
}

func TestNewWithEndpointsReadsTheQuotaHeaders(t *testing.T) {
	p, err := NewWithEndpoints(DefaultEndpoints, testManifest)
	if err != nil || len(p.quota) != len(deviceQuota(t).Headers) {
		t.Fatalf("manifest.json: %v %+v", err, p)
	}
	noQuota, _ := json.Marshal(manifest.Manifest{AccountTypes: []manifest.AccountType{{ID: TypeDevice}}})
	for name, raw := range map[string][]byte{"not json": []byte("{"), "no quota headers": noQuota} {
		if _, err := NewWithEndpoints(DefaultEndpoints, raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

package growth

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// TestValidateSettings: every value the settings JSON Schema refused (one
// counterexample per constraint, written out here) is still refused, at its
// field, by ValidateSettings and Configure alike; the plugin's own checks
// (minimum over maximum, time zone) too; the default set and real settings
// pass.
func TestValidateSettings(t *testing.T) {
	h := pluginsdktest.Start(t, New(), pluginsdktest.Options{Manifest: manifestJSON})
	refused := []struct{ settings, field, code string }{
		{`{"referral_enabled":"yes"}`, "referral_enabled", "type"},
		{`{"referral_rate_percent":-1}`, "referral_rate_percent", "minimum"},
		{`{"referral_rate_percent":100.5}`, "referral_rate_percent", "maximum"},
		{`{"referral_rate_percent":"10"}`, "referral_rate_percent", "type"},
		{`{"referral_max_per_invitee":-0.01}`, "referral_max_per_invitee", "minimum"},
		{`{"referral_duration_days":-1}`, "referral_duration_days", "minimum"},
		{`{"referral_duration_days":1.5}`, "referral_duration_days", "type"},
		{`{"checkin_enabled":1}`, "checkin_enabled", "type"},
		{`{"checkin_min_quota":-1}`, "checkin_min_quota", "minimum"},
		{`{"checkin_max_quota":-1}`, "checkin_max_quota", "minimum"},
		{`{"checkin_timezone":8}`, "checkin_timezone", "type"},
		{`{"referral_rate":10}`, "referral_rate", "unknown"},
		{`[]`, "", "invalid_json"},
		{`{"checkin_min_quota":0.5,"checkin_max_quota":0.1}`, "checkin_max_quota", "invalid"},
		{`{"checkin_timezone":"Mars/Base"}`, "checkin_timezone", "invalid"},
	}
	for _, c := range refused {
		resp, err := h.ValidateSettings(c.settings)
		if err != nil {
			t.Fatalf("%s: %v", c.settings, err)
		}
		if errs := resp.GetErrors(); len(errs) != 1 || errs[0].GetField() != c.field || errs[0].GetCode() != c.code {
			t.Errorf("%s: errors = %v, want %s %s", c.settings, errs, c.field, c.code)
		}
		if _, ok := pluginsdk.AsFieldErrors(New().Configure(context.Background(), pluginsdk.Config{JSON: []byte(c.settings)})); !ok {
			t.Errorf("%s: Configure accepted it", c.settings)
		}
	}
	for _, ok := range []string{
		`{}`,
		// The set the console form saves untouched (its defaults).
		`{"referral_enabled":true,"referral_rate_percent":10,"referral_max_per_invitee":0,"referral_duration_days":0,"checkin_enabled":true,"checkin_min_quota":0.01,"checkin_max_quota":0.05,"checkin_timezone":"UTC"}`,
		`{"referral_enabled":false,"referral_rate_percent":100,"referral_max_per_invitee":12.5,"referral_duration_days":30,"checkin_enabled":true,"checkin_min_quota":0.1,"checkin_max_quota":0.1,"checkin_timezone":"Asia/Shanghai"}`,
	} {
		resp, err := h.ValidateSettings(ok)
		if err != nil || len(resp.GetErrors()) != 0 || resp.GetNormalizedConfigJson() != "" {
			t.Errorf("%s: %v %v", ok, resp.GetErrors(), err)
		}
	}
}

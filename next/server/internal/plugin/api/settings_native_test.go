package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/secret"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/install"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/pkg/pkgtest"
)

// settingsCheck is a plugin's ValidateSettings: it records what it was
// given and answers answer (or err).
type settingsCheck struct {
	got    []string
	answer *pluginv1.ValidateSettingsResponse
	err    error
}

func (s *settingsCheck) ValidateSettings(_ context.Context, in *pluginv1.ValidateSettingsRequest) (*pluginv1.ValidateSettingsResponse, error) {
	s.got = append(s.got, in.GetConfigJson())
	if s.err != nil {
		return nil, s.err
	}
	return s.answer, nil
}

// installNativeSettings installs guard with native settings that declare
// their secrets in ui.settings.secrets (no schema).
func installNativeSettings(t *testing.T, h *harness, root pkgtest.Key) {
	t.Helper()
	guard := pkgtest.Guard("guard", "0.1.0", "sub2api")
	guard.UI.Settings = &manifest.Form{Mode: "native", Component: "GuardSettings", Permission: "stats:read", Secrets: []manifest.SettingsSecret{
		{Path: "webhook_secret"}, {Path: "notify/url", Sensitive: true}}}
	code, out := h.do("POST", "/plugins/upload", "admin", fileBody(t, pkgtest.Build(guard, root)))
	require.Equal(t, http.StatusOK, code, out)
	consent := install.ConsentRequest{Grants: []install.GrantInput{{Permission: "db.schema"}, {Permission: "gateway.hook"},
		{Permission: "events"}, {Permission: "jobs"}, {Permission: "routes.admin"}, {Permission: "ui.menu"}, {Permission: "ui.native"}}}
	code, out = h.do("POST", "/plugins/guard/versions/0.1.0/consent", "admin", consent)
	require.Equal(t, http.StatusOK, code, out)
}

func (h *harness) storedSettings() string {
	h.t.Helper()
	var enc []byte
	require.NoError(h.t, h.db.SQL.QueryRowContext(context.Background(), `SELECT config_enc FROM plugins WHERE key = 'guard'`).Scan(&enc))
	cipher, _ := secret.New(bytes.Repeat([]byte{7}, 32))
	plain, err := cipher.Decrypt(enc, ConfigAAD("guard"))
	require.NoError(h.t, err)
	return string(plain)
}

func fieldsOf(out map[string]any) map[string]string {
	e, _ := out["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	list, _ := d["fields"].([]any)
	got := map[string]string{}
	for _, f := range list {
		m := f.(map[string]any)
		got[m["field"].(string)], _ = m["code"].(string)
	}
	return got
}

// Native settings without a schema mask what ui.settings.secrets names and
// nothing else; masks keep the stored values and the plugin-permission
// holder re-enters a kept credential like with a schema (§75.1).
func TestNativeSettingsMaskDeclaredSecrets(t *testing.T) {
	h, root := newHarness(t)
	installNativeSettings(t, h, root)

	code, out := h.do("PUT", "/plugins/guard/settings", "admin", map[string]any{"values": map[string]any{
		"threshold": 3, "webhook_secret": "s3cret", "notify": map[string]any{"url": "https://hooks.example/x", "name": "n"}}})
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "native", data(out)["mode"])
	assert.Equal(t, map[string]any{"threshold": 3.0, "webhook_secret": Mask, "notify": map[string]any{"url": Mask, "name": "n"}}, data(out)["values"])
	assert.Equal(t, []any{"notify/url", "webhook_secret"}, data(out)["secret_fields"])

	code, out = h.do("PUT", "/plugins/guard/settings", "admin", map[string]any{"values": map[string]any{
		"threshold": 4, "webhook_secret": Mask, "notify": map[string]any{"url": Mask, "name": "n"}}})
	require.Equal(t, http.StatusOK, code, out)
	assert.JSONEq(t, `{"threshold":4,"webhook_secret":"s3cret","notify":{"url":"https://hooks.example/x","name":"n"}}`, h.storedSettings())

	code, out = h.do("PUT", "/plugins/guard/settings", "viewer", map[string]any{"values": map[string]any{
		"threshold": 5, "webhook_secret": Mask, "notify": map[string]any{"url": Mask, "name": "n"}}})
	require.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, map[string]string{"values/webhook_secret": "reenter"}, fieldsOf(out))

	// Declaring no secrets masks nothing.
	_, err := h.db.SQL.ExecContext(context.Background(),
		`UPDATE plugin_versions SET manifest = manifest #- '{ui,settings,secrets}' WHERE plugin_key = 'guard'`)
	require.NoError(t, err)
	code, out = h.do("GET", "/plugins/guard/settings", "admin", nil)
	require.Equal(t, http.StatusOK, code, out)
	assert.Equal(t, "s3cret", data(out)["values"].(map[string]any)["webhook_secret"])
	assert.Equal(t, []any{}, data(out)["secret_fields"])
}

// PUT /plugins/:key/settings asks the enabled plugin (ValidateSettings) with
// the masked secrets restored: its field errors come back below values, its
// normalized object is stored; UNIMPLEMENTED and a plugin that is not in the
// generation accept.
func TestSettingsValidatedByPlugin(t *testing.T) {
	h, root := newHarness(t)
	installNativeSettings(t, h, root)
	put := func(values map[string]any) (int, map[string]any) {
		return h.do("PUT", "/plugins/guard/settings", "admin", map[string]any{"values": values})
	}

	// Not in the generation: stored as submitted.
	code, out := put(map[string]any{"threshold": 3, "webhook_secret": "s3cret"})
	require.Equal(t, http.StatusOK, code, out)

	check := &settingsCheck{answer: &pluginv1.ValidateSettingsResponse{Errors: []*pluginv1.FieldError{
		{Field: "notify/url", Code: "pattern", Message: "bad url"}, {Field: "", Code: "invalid", Message: "whole"}}}}
	h.validators["guard"] = check
	code, out = put(map[string]any{"threshold": 4, "webhook_secret": Mask, "notify": map[string]any{"url": "x"}})
	require.Equal(t, http.StatusBadRequest, code, out)
	assert.Equal(t, map[string]string{"values/notify/url": "pattern", "values": "invalid"}, fieldsOf(out))
	require.Len(t, check.got, 1)
	assert.JSONEq(t, `{"threshold":4,"webhook_secret":"s3cret","notify":{"url":"x"}}`, check.got[0], "secrets restored before the plugin sees them")
	assert.JSONEq(t, `{"threshold":3,"webhook_secret":"s3cret"}`, h.storedSettings(), "nothing stored")

	check.answer = &pluginv1.ValidateSettingsResponse{NormalizedConfigJson: `{"threshold":5,"webhook_secret":"s3cret","mode":"on"}`}
	code, out = put(map[string]any{"threshold": "5", "webhook_secret": Mask})
	require.Equal(t, http.StatusOK, code, out)
	assert.JSONEq(t, `{"threshold":5,"webhook_secret":"s3cret","mode":"on"}`, h.storedSettings())
	assert.Equal(t, map[string]any{"threshold": 5.0, "webhook_secret": Mask, "mode": "on"}, data(out)["values"])

	check.answer = &pluginv1.ValidateSettingsResponse{NormalizedConfigJson: `[1]`}
	code, out = put(map[string]any{"threshold": 6})
	require.Equal(t, http.StatusServiceUnavailable, code, out)

	check.err = status.Error(codes.Unimplemented, "plugin does not validate its settings")
	code, out = put(map[string]any{"threshold": 7, "webhook_secret": Mask})
	require.Equal(t, http.StatusOK, code, out)
	assert.JSONEq(t, `{"threshold":7,"webhook_secret":"s3cret"}`, h.storedSettings())

	check.err = status.Error(codes.Unavailable, "down")
	code, out = put(map[string]any{"threshold": 8})
	require.NotEqual(t, http.StatusOK, code, out)
	assert.JSONEq(t, `{"threshold":7,"webhook_secret":"s3cret"}`, h.storedSettings())
}

// has_settings: the current version declares ui.settings, whatever its mode.
func TestHasSettingsAnyMode(t *testing.T) {
	h, root := newHarness(t)
	installNativeSettings(t, h, root)
	code, out := h.do("GET", "/plugins", "admin", nil)
	require.Equal(t, http.StatusOK, code, out)
	list, _ := out["data"].([]any)
	require.Len(t, list, 1)
	assert.Equal(t, true, list[0].(map[string]any)["has_settings"])
	raw, _ := json.Marshal(out)
	assert.NotContains(t, string(raw), "s3cret")
}

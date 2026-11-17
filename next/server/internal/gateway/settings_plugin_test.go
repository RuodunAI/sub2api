package gateway

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/event"
)

// Every gateway settings section is read for plugins as the core runs it,
// and saving one records settings.changed naming it in the same transaction
// (CONTRACTS §106).
func TestSettingsForPlugins(t *testing.T) {
	ctx := context.Background()
	e := newDBEnv(t, func(d *Deps) { d.Events = event.NewPublisher(d.DB) })
	readers := SettingsReaders(e.db)
	read := func(section string) protocol.SettingsValues {
		t.Helper()
		v, err := readers[section](ctx)
		require.NoError(t, err)
		return v
	}
	events := func() []string {
		t.Helper()
		var payloads []string
		rows, err := e.db.SQL.QueryContext(ctx, `SELECT payload::text FROM events WHERE type = 'settings.changed' ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var p string
			require.NoError(t, rows.Scan(&p))
			payloads = append(payloads, p)
		}
		return payloads
	}

	// Defaults, with no rows.
	require.Equal(t, protocol.StickySettings{Enabled: true, DefaultTTLSeconds: 3600}, read(protocol.SettingsSticky))
	require.Equal(t, protocol.SchedulingSettings{Metric: balanceMetricSessions}, read(protocol.SettingsScheduling))
	require.Equal(t, protocol.FailurePolicySettings{}, read(protocol.SettingsFailurePolicy))
	gw := read(protocol.SettingsGateway).(protocol.GatewaySettings)
	require.Equal(t, 3, gw.MaxAttempts)
	require.Equal(t, 300, gw.PlatformHotpathTimeoutMs)
	require.Equal(t, map[string]int{claudeDefaultKey: defaultClaudeMaxTokens}, gw.ClaudeMaxTokens.Models)
	ad := read(protocol.SettingsAutoDisable).(protocol.AutoDisableSettings)
	require.True(t, ad.Enabled)
	require.Equal(t, "401", ad.StatusCodes)
	require.Empty(t, events())

	for _, put := range []struct {
		path string
		body any
	}{
		{"/settings/sticky", map[string]any{"enabled": false}},
		{"/settings/gateway", map[string]any{"max_attempts": 4}},
		{"/settings/auto-disable", map[string]any{"enabled": false}},
		{"/settings/scheduling", map[string]any{"metric": "rpm"}},
	} {
		code, res := e.api("PUT", put.path, put.body)
		require.Equal(t, 200, code, res.Raw)
	}
	require.Equal(t, []string{`{"sections": ["sticky"]}`, `{"sections": ["gateway"]}`, `{"sections": ["auto_disable"]}`,
		`{"sections": ["scheduling"]}`}, events())
	require.False(t, read(protocol.SettingsSticky).(protocol.StickySettings).Enabled)
	require.Equal(t, 4, read(protocol.SettingsGateway).(protocol.GatewaySettings).MaxAttempts)
	require.False(t, read(protocol.SettingsAutoDisable).(protocol.AutoDisableSettings).Enabled)
	require.Equal(t, "rpm", read(protocol.SettingsScheduling).(protocol.SchedulingSettings).Metric)

	// A published failure policy: its switches, not its rules.
	_, res := e.api("GET", "/settings/failure-policy", nil)
	code, res := e.api("PUT", "/settings/failure-policy", map[string]any{"expected_revision": 0, "policy": res.Get("data.policy").Value()})
	require.Equal(t, 200, code, res.Raw)
	require.Equal(t, `{"sections": ["failure_policy"]}`, events()[4])
	fp := read(protocol.SettingsFailurePolicy).(protocol.FailurePolicySettings)
	require.True(t, fp.Configured)
	require.Equal(t, int64(1), fp.Revision)
	require.Equal(t, 3, fp.MaxRetries) // the draft takes max_attempts - 1

	// A refused save records nothing.
	code, _ = e.api("PUT", "/settings/auto-disable", map[string]any{"enabled": true})
	require.NotEqual(t, 200, code)
	require.Len(t, events(), 5)
}

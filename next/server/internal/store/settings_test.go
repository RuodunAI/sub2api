package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// SettingValue reads what PatchSettingJSON stored, and a missing row is an
// IsNoRows error that the loaders turn into their defaults.
func TestSettingValueRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := t.Context()
	db := testutil.DB(t)
	const key = "store.test_setting_value"

	_, err := store.SettingValue(store.G(ctx, db.Pool), key)
	require.True(t, store.IsNoRows(err), "a missing row: %v", err)

	into := struct {
		A int `json:"a"`
		B int `json:"b"`
	}{B: 2}
	require.NoError(t, store.PatchSettingJSON(ctx, db, key, nil, map[string]int{"a": 1}, &into))
	raw, err := store.SettingValue(store.G(ctx, db.Pool), key)
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1,"b":2}`, string(raw))

	require.NoError(t, db.Tx(ctx, func(tx pgx.Tx) error {
		raw, err := store.SettingValue(store.G(ctx, tx), key, store.ForUpdate)
		require.JSONEq(t, `{"a":1,"b":2}`, string(raw))
		return err
	}))
}

package store_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

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

	_, err := store.SettingValue(store.G(ctx, db.Gorm), key)
	require.True(t, store.IsNoRows(err), "a missing row: %v", err)

	into := struct {
		A int `json:"a"`
		B int `json:"b"`
	}{B: 2}
	require.NoError(t, store.PatchSettingJSON(ctx, db, key, nil, map[string]int{"a": 1}, &into))
	raw, err := store.SettingValue(store.G(ctx, db.Gorm), key)
	require.NoError(t, err)
	require.JSONEq(t, `{"a":1,"b":2}`, string(raw))

	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
		raw, err := store.SettingValue(store.G(ctx, tx), key, store.ForUpdate())
		require.JSONEq(t, `{"a":1,"b":2}`, string(raw))
		return err
	}))
}

// UpdateSettingJSONTx reads, merges and writes under a row lock that only a
// transaction keeps: on the pool the lock would end with the SELECT and two
// concurrent updates could both read the old document (G1 of the GORM
// review). It refuses a handle that runs on no transaction before touching
// the row.
func TestUpdateSettingJSONTxNeedsATransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := t.Context()
	db := testutil.DB(t)
	const key = "store.test_update_needs_tx"
	called := false
	update := func(json.RawMessage) (json.RawMessage, error) { called = true; return json.RawMessage(`{"a":1}`), nil }

	_, err := store.UpdateSettingJSONTx(ctx, db.Gorm, key, nil, update)
	require.ErrorIs(t, err, store.ErrNoTx)
	require.False(t, called)
	_, err = store.SettingValue(store.G(ctx, db.Gorm), key)
	require.True(t, store.IsNoRows(err), "the pool call wrote the row: %v", err)

	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
		_, err := store.UpdateSettingJSONTx(ctx, tx, key, nil, update)
		return err
	}))
	require.True(t, called)
}

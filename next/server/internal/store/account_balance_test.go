package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestAccountBalanceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := t.Context()
	db := testutil.DB(t)
	id := createTestAccount(t, db)

	b, err := db.AccountBalance(ctx, id)
	require.NoError(t, err)
	require.Nil(t, b, "no snapshot yet")

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.UpsertBalance(ctx, &store.BalanceSnapshot{AccountID: id, AmountMicros: 5, Currency: "USD",
		Source: store.BalanceActive, UpdatedAt: &at}))
	b, err = db.AccountBalance(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, b)
	require.Equal(t, int64(5), b.AmountMicros)
	require.True(t, b.UpdatedAt.Equal(at))
	require.Nil(t, b.ErrorAt, "a NULL timestamp reads as nil")

	require.NoError(t, db.UpsertBalance(ctx, &store.BalanceSnapshot{AccountID: id, AmountMicros: 5, Currency: "USD",
		Source: store.BalanceActive, UpdatedAt: &at, Error: "boom", ErrorAt: &at}))
	all, err := db.AccountBalances(ctx, []int64{id, id + 1})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, "boom", all[id].Error)
	require.True(t, all[id].ErrorAt.Equal(at))

	require.NoError(t, db.DeleteBalance(ctx, id))
	b, err = db.AccountBalance(ctx, id)
	require.NoError(t, err)
	require.Nil(t, b)
}

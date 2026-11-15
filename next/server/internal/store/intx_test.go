package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// RequireTx passes the handles of a transaction, however they were opened
// or wrapped, and refuses the pool, a connection, DryRun and nil.
func TestRequireTx(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	inTx := func(q *gorm.DB) error { return store.RequireTx(store.G(ctx, q)) }

	for name, q := range map[string]*gorm.DB{"pool": db.Gorm, "dry run": store.DryRun(), "nil": nil} {
		assert.ErrorIs(t, store.RequireTx(q), store.ErrNoTx, name)
		assert.False(t, store.InTx(q), name)
	}
	require.NoError(t, db.Tx(ctx, inTx))
	require.NoError(t, db.TxOptions(ctx, store.Snapshot(), inTx))
	traced, _ := testutil.Traced(db)
	require.NoError(t, traced.Tx(ctx, inTx), "a traced transaction is one")
	require.NoError(t, db.WithConn(ctx, func(conn *gorm.DB) error {
		assert.ErrorIs(t, inTx(conn), store.ErrNoTx, "a connection outside a transaction")
		return conn.Transaction(inTx)
	}))
}

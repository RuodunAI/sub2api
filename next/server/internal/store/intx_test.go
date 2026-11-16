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

// XactLock takes its lock in a transaction only, where it holds until the
// end: a second transaction waits for it (core.OpenDraftsLock is one), and
// TryXactLock does not get it meanwhile. The lock's id is the key's
// hashtextextended, so "open-drafts:7" is the lock that 0.1.288 took as
// hashtextextended('open-drafts:' || 7::bigint::text, 0).
func TestXactLock(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	const key = "open-drafts:7"
	require.ErrorIs(t, store.XactLock(ctx, db.Gorm, key), store.ErrNoTx)
	_, err := store.TryXactLock(ctx, db.Gorm, key)
	require.ErrorIs(t, err, store.ErrNoTx)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Tx(ctx, func(tx *gorm.DB) error {
			if err := store.XactLock(ctx, tx, key); err != nil {
				return err
			}
			var id bool
			if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND objsubid = 1
				AND (classid::bigint << 32 | objid::bigint) = hashtextextended('open-drafts:' || 7::bigint::text, 0))`).Scan(&id).Error; err != nil {
				return err
			}
			assert.True(t, id, "the lock's id is the key's hashtextextended")
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
		free, err := store.TryXactLock(ctx, tx, key)
		assert.False(t, free, "the lock is held until the first transaction ends")
		return err
	}))
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error { return store.XactLock(ctx, tx, key) }))
	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
		free, err := store.TryXactLock(ctx, tx, key)
		assert.True(t, free, "a free lock is taken")
		return err
	}))
}

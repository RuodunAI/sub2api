package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// TestGORMExecDoesNotDeadlockOnRowLocks guards the bridge's pool settings.
//
// database/sql hands the driverConn of an Exec back to its pool and only
// then reads RowsAffected, under that driverConn's mutex (execDC, then
// driverResult.RowsAffected; gorm reads it right after ExecContext). With
// idle connections kept, another goroutine can take that driverConn in
// between and run a statement that waits in PostgreSQL for a row lock of
// the first goroutine's transaction, holding the mutex all along: the first
// goroutine never gets past RowsAffected, so it never commits, and
// PostgreSQL sees no deadlock to break.
//
// The race window is a few hundred nanoseconds, so the test widens it with
// the bridge's own *sql.DB: it runs the transaction's Exec, lets pool
// statements take every idle driverConn and wait for the row lock, and
// only then reads RowsAffected, as gorm would have.
func TestGORMExecDoesNotDeadlockOnRowLocks(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `INSERT INTO widgets (name) VALUES ('locked')`)
	require.NoError(t, err)
	const update = `UPDATE widgets SET tag = tag || 'x' WHERE name = 'locked'`

	tx, err := db.Pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	g := store.G(ctx, tx)
	sqlDB, err := g.DB()
	require.NoError(t, err)
	res, err := sqlDB.ExecContext(g.Statement.Context, update) // takes the row lock
	require.NoError(t, err)

	// No driverConn outlives its statement.
	assert.Zero(t, sqlDB.Stats().Idle, "the bridge keeps idle driverConns")

	// Pool updates take whatever is idle (at least one) and wait for the
	// row lock, each holding its driverConn's mutex.
	poolCtx := store.G(ctx, db.Pool).Statement.Context
	var waiters sync.WaitGroup
	for range max(sqlDB.Stats().Idle, 1) {
		waiters.Go(func() { _, _ = sqlDB.ExecContext(poolCtx, update) })
	}
	require.Eventually(t, func() bool {
		var n int
		err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		return err == nil && n > 0 && sqlDB.Stats().Idle == 0
	}, 10*time.Second, 10*time.Millisecond)

	read := make(chan error, 1)
	go func() {
		_, err := res.RowsAffected()
		read <- err
	}()
	select {
	case err := <-read:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		// Free the waiters, and with them the reader, before failing.
		_ = tx.Rollback(ctx)
		<-read
		t.Fatal("RowsAffected waits for a driverConn that a statement waiting on this transaction's row lock holds")
	}
	require.NoError(t, tx.Commit(ctx))
	waiters.Wait()
}

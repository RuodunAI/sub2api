package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// An Exec on the pool reads its result before its connection goes back.
// database/sql reads RowsAffected after, under the connection's mutex, which
// the connection's next statement holds while it waits for a row lock: with
// that lock held by a transaction that waits for the Exec, nothing moves and
// PostgreSQL sees no deadlock. GORM reads the result right after the Exec,
// so the test widens the window on the pool GORM runs on: the Exec, then a
// statement that takes its connection and waits, then the read.
func TestPoolExecDoesNotWaitForRowLocks(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	require.NoError(t, store.G(ctx, db.Gorm).Create(&[]widget{{Name: "locked"}, {Name: "free"}}).Error)
	pool, err := store.OpenSized(ctx, testutil.DSN(db), 2)
	require.NoError(t, err)
	defer pool.Close()
	sqlDB, err := pool.Gorm.DB()
	require.NoError(t, err)
	assert.Same(t, pool.SQL, sqlDB, "GORM's *sql.DB is the pool's")

	// T holds the row lock on one connection.
	tx, err := testutil.Begin(ctx, pool)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, tx.Exec(`UPDATE widgets SET tag = 't' WHERE name = 'locked'`).Error)

	// A runs on the other, which then goes back to the pool.
	res, err := store.G(ctx, pool.Gorm).Statement.ConnPool.ExecContext(ctx, `UPDATE widgets SET tag = 'a' WHERE name = 'free'`)
	require.NoError(t, err)

	// B takes it and waits for T's lock.
	waiter := make(chan error, 1)
	go func() { waiter <- store.G(ctx, pool.Gorm).Exec(`UPDATE widgets SET tag = 'b' WHERE name = 'locked'`).Error }()
	require.Eventually(t, func() bool {
		var n int
		err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		return err == nil && n == 1
	}, 10*time.Second, 10*time.Millisecond)

	read := make(chan int64, 1)
	go func() {
		n, _ := res.RowsAffected()
		read <- n
	}()
	select {
	case n := <-read:
		assert.EqualValues(t, 1, n)
	case <-time.After(5 * time.Second):
		// Free B, and with it the read, before failing.
		tx.Rollback()
		<-read
		t.Fatal("A's RowsAffected waits for B, which waits for T's row lock")
	}
	require.NoError(t, tx.Rollback().Error)
	require.NoError(t, <-waiter)
}

// Times parses DateStyle ISO's text; open pins it over the database's
// default and the DSN's.
func TestTimesScanWhateverTheDateStyle(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	var name string
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name))
	_, err := db.SQL.ExecContext(ctx, `ALTER DATABASE `+store.QuoteIdent(name)+` SET datestyle = 'SQL, DMY'`)
	require.NoError(t, err)
	at := time.Date(2024, 5, 1, 10, 0, 0, 123456000, time.UTC)
	read := `SELECT current_setting('datestyle'), ARRAY[?::timestamptz]`

	dsn := testutil.DSN(db)
	for _, dsn := range []string{
		dsn,
		dsn + "&DateStyle=German",
		dsn + "&options=-c%20datestyle%3DPostgres,MDY",
	} {
		s, err := store.OpenSized(ctx, dsn, 1)
		require.NoError(t, err)
		var style string
		var got store.Times
		err = store.G(ctx, s.Gorm).Raw(read, at).Row().Scan(&style, &got)
		s.Close()
		require.NoError(t, err, dsn)
		assert.True(t, strings.HasPrefix(style, "ISO"), "DateStyle %q", style)
		require.Len(t, got, 1)
		assert.True(t, at.Equal(got[0]), "%v", got)
	}

	// What the pin prevents.
	err = db.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Exec(`SET LOCAL datestyle = 'SQL, DMY'`).Error; err != nil {
			return err
		}
		var style string
		var got store.Times
		return tx.Raw(read, at).Row().Scan(&style, &got)
	})
	require.ErrorContains(t, err, "invalid timestamptz")
	assert.NotErrorIs(t, err, sql.ErrNoRows)
}

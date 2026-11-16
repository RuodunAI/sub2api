package store_test

import (
	"context"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func widgetNames(t *testing.T, db *store.DB) []string {
	t.Helper()
	var names []string
	require.NoError(t, store.G(context.Background(), db.Gorm).Model(&widget{}).Order("name").Pluck("name", &names).Error)
	return names
}

// Tx commits on nil, rolls back on an error, and rolls back on a panic,
// which it raises again.
func TestTxCommitRollbackPanic(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	insert := func(name string) func(*gorm.DB) error {
		return func(tx *gorm.DB) error { return store.G(ctx, tx).Create(&widget{Name: name}).Error }
	}

	require.NoError(t, db.Tx(ctx, insert("committed")))
	err := db.Tx(ctx, func(tx *gorm.DB) error {
		if err := insert("failed")(tx); err != nil {
			return err
		}
		return errRollback
	})
	assert.ErrorIs(t, err, errRollback)
	assert.PanicsWithValue(t, "boom", func() {
		_ = db.Tx(ctx, func(tx *gorm.DB) error {
			_ = insert("panicked")(tx)
			panic("boom")
		})
	})
	assert.Equal(t, []string{"committed"}, widgetNames(t, db))
	assert.Zero(t, db.SQL.Stats().InUse, "every transaction gave its connection back")
}

// TxOptions passes the isolation level and read-only access to the server.
func TestTxOptions(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	var iso, ro string
	require.NoError(t, db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		return store.G(ctx, tx).Raw(`SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).Row().Scan(&iso, &ro)
	}))
	assert.Equal(t, [2]string{"repeatable read", "on"}, [2]string{iso, ro})

	err := db.TxOptions(ctx, store.ReadOnly(), func(tx *gorm.DB) error {
		return store.G(ctx, tx).Create(&widget{Name: "ro"}).Error
	})
	assert.Equal(t, "25006", store.SQLState(err), "read_only_sql_transaction: %v", err)

	// A snapshot does not see what commits after its first read.
	err = db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		before, _ := countWidgets(t, ctx, tx)
		require.NoError(t, store.G(ctx, db.Gorm).Create(&widget{Name: "later"}).Error)
		after, _ := countWidgets(t, ctx, tx)
		assert.Equal(t, before, after)
		return nil
	})
	require.NoError(t, err)
}

// WithConn runs every statement on one connection, where a session lock
// lives, and opens transactions on it.
func TestWithConn(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	err := db.WithConn(ctx, func(conn *gorm.DB) error {
		var pid, again, inTx int
		g := store.G(ctx, conn)
		require.NoError(t, g.Raw(`SELECT pg_backend_pid()`).Row().Scan(&pid))
		require.NoError(t, g.Exec(`SELECT pg_advisory_lock(42)`).Error)
		require.NoError(t, store.G(ctx, conn).Raw(`SELECT pg_backend_pid()`).Row().Scan(&again))
		require.NoError(t, conn.Transaction(func(tx *gorm.DB) error {
			require.NotNil(t, store.SQLTx(tx))
			if err := store.G(ctx, tx).Create(&widget{Name: "on-conn"}).Error; err != nil {
				return err
			}
			return store.G(ctx, tx).Raw(`SELECT pg_backend_pid()`).Row().Scan(&inTx)
		}))
		assert.Equal(t, [2]int{pid, pid}, [2]int{again, inTx})

		// Another connection does not get the session lock.
		var got bool
		require.NoError(t, store.G(ctx, db.Gorm).Raw(`SELECT pg_try_advisory_lock(42)`).Row().Scan(&got))
		assert.False(t, got)
		return g.Exec(`SELECT pg_advisory_unlock(42)`).Error
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"on-conn"}, widgetNames(t, db))
	assert.Zero(t, db.SQL.Stats().InUse, "the connection went back to the pool")
}

// A migration file of several statements runs in one Exec: pgx sends a
// statement without arguments with the simple protocol, which allows them.
func TestMigrateRunsMultiStatementFiles(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	fsys := fstest.MapFS{"0001_multi.sql": {Data: []byte(`
		CREATE TABLE multi (id int PRIMARY KEY, note text);
		INSERT INTO multi VALUES (1, 'what? @who');
		INSERT INTO multi VALUES (2, $$it's $1$$);`)}}
	done, err := store.Migrate(ctx, db, fsys, memTracker{}, store.MigrateOptions{LockKey: 43})
	require.NoError(t, err)
	assert.Equal(t, []string{"0001_multi.sql"}, done)
	var notes []string
	require.NoError(t, store.G(ctx, db.Gorm).Raw(`SELECT note FROM multi ORDER BY id`).Scan(&notes).Error)
	assert.Equal(t, []string{"what? @who", "it's $1"}, notes, "the text reached the server as written")
}

// Runs on two pools serialize on the migration lock; each file runs once.
func TestMigrateSerializesRuns(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	other, err := store.Open(ctx, testutil.DSN(db))
	require.NoError(t, err)
	defer other.Close()
	fsys := fstest.MapFS{"0001.sql": {Data: []byte(`INSERT INTO widgets (name) VALUES ('once')`)}}
	tr := &syncTracker{applied: map[string]string{}}
	var wg sync.WaitGroup
	for _, pool := range []*store.DB{db, other} {
		wg.Go(func() {
			_, err := store.Migrate(ctx, pool, fsys, tr, store.MigrateOptions{LockKey: 44})
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Equal(t, []string{"once"}, widgetNames(t, db))
}

// syncTracker is a memTracker safe for concurrent runs.
type syncTracker struct {
	mu      sync.Mutex
	applied map[string]string
}

func (*syncTracker) Ensure(context.Context, *gorm.DB) error { return nil }
func (s *syncTracker) Applied(context.Context, *gorm.DB) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.applied {
		out[k] = v
	}
	return out, nil
}
func (s *syncTracker) Record(_ context.Context, _ *gorm.DB, id, checksum string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied[id] = checksum
	return nil
}

// SUB2API_PG_MAX_CONNS sizes the pool.
func TestOpenPoolSize(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	t.Setenv("SUB2API_PG_MAX_CONNS", "3")
	sized, err := store.Open(ctx, testutil.DSN(db))
	require.NoError(t, err)
	defer sized.Close()
	assert.Equal(t, 3, sized.MaxConns())
	require.NoError(t, sized.Ping(ctx))
}

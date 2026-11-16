package migrations_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/migrations"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The River schema applied by the core migrations is the one the pinned
// River expects: rivermigrate finds every version and nothing to do.
func TestRiverSchemaIsCurrent(t *testing.T) {
	db := testutil.DB(t)
	ctx := t.Context()
	migrator, err := rivermigrate.New(riverdatabasesql.New(db.SQL), nil)
	require.NoError(t, err)
	res, err := migrator.Validate(ctx, nil)
	require.NoError(t, err)
	require.True(t, res.OK, "rivermigrate validation: %v", res.Messages)
	existing, err := migrator.ExistingVersions(ctx)
	require.NoError(t, err)
	require.Len(t, existing, len(migrator.AllVersions()))
	for _, table := range []string{"river_job", "river_leader", "river_queue", "river_notification", "river_migration", "scheduled_tasks"} {
		var ok bool
		require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok))
		require.True(t, ok, table)
	}
}

// The core runs its migrations with the same non-superuser role it serves
// with (Supabase); River's schema must not need more.
func TestCoreMigrationsAsNonSuperuser(t *testing.T) {
	base := testpg.URL(t)
	ctx := context.Background()
	admin, done, err := testutil.Connect(ctx, base)
	require.NoError(t, err)
	defer done()
	role := fmt.Sprintf("mig_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)
	_, err = admin.ExecContext(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD 'pw' NOSUPERUSER NOCREATEDB NOCREATEROLE")
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+role+" OWNER "+role)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, done, err := testutil.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer done()
		_, _ = c.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+role+" WITH (FORCE)")
		_, _ = c.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.User = url.UserPassword(role, "pw")
	u.Path = "/" + role
	db, err := store.Open(ctx, u.String())
	require.NoError(t, err)
	defer db.Close()
	var super bool
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super))
	require.False(t, super)
	for range 2 {
		_, err = store.Migrate(ctx, db, migrations.FS, store.CoreTracker{}, store.MigrateOptions{LockKey: store.CoreMigrationLockKey})
		require.NoError(t, err)
	}
	var versions int
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM river_migration WHERE line = 'main'`).Scan(&versions))
	require.Equal(t, 8, versions)
}

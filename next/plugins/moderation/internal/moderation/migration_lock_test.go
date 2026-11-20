package moderation

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// The migrations of 0.10.0 and 0.11.0 alter events, which long statistics
// queries read: behind such a query a migration fails after lock_timeout
// instead of queueing (and making the gateway's inserts queue behind it).
// The failed file leaves nothing behind and runs again on a retry.
func TestMigrationsFailFastOnLock(t *testing.T) {
	dsn, schema := pluginsdktest.NewSchema(t, "plg_moderation_t")
	// unchanged counts what the files of a step add or drop: it is what
	// they find while they fail.
	for _, step := range []struct {
		files     []string
		unchanged string
		want      int
	}{
		{[]string{"0011_flow_scope_source.sql", "0012_lists_level.sql"}, `(table_name, column_name) IN
			(('plg_moderation__flows', 'scope'), ('plg_moderation__events', 'flow_source'), ('plg_moderation__watchlist', 'locked'), ('plg_moderation__events', 'level'))`, 0},
		{[]string{"0013_contract.sql"}, `(table_name, column_name) IN
			(('plg_moderation__events', 'ai_verdict'), ('plg_moderation__watchlist', 'latest_trend'), ('plg_moderation__user_analyses', 'id'), ('plg_moderation__config_state', 'lists_imported'))`, 3},
	} {
		first := step.files[0]
		pluginsdktest.ApplyMigrations(t, dsn, schema, migrationDir(t, func(n string) bool { return n < first }))
		// A long query on events: its ACCESS SHARE lock conflicts with ALTER TABLE.
		reader := connect(t, dsn).Begin()
		if reader.Error != nil {
			t.Fatal(reader.Error)
		}
		if err := reader.Exec(`SELECT count(*) FROM ` + ident(schema, "plg_moderation__events")).Error; err != nil {
			t.Fatal(err)
		}
		for _, name := range step.files {
			body, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			var pe interface{ SQLState() string }
			err = connect(t, dsn).Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("SET LOCAL search_path TO " + ident(schema)).Error; err != nil {
					return err
				}
				return tx.Exec(string(body)).Error
			})
			if !errors.As(err, &pe) || pe.SQLState() != "55P03" || time.Since(start) > 15*time.Second {
				t.Fatalf("%s behind a reader: %v after %s, want lock_not_available after 10s", name, err, time.Since(start))
			}
			var cols int
			if err := connect(t, dsn).Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema = ? AND `+step.unchanged,
				schema).Row().Scan(&cols); err != nil || cols != step.want {
				t.Fatalf("%s: %d of the columns it changes, want %d (%v)", name, cols, step.want, err)
			}
		}
		if err := reader.Rollback().Error; err != nil {
			t.Fatal(err)
		}
	}
	pluginsdktest.ApplyMigrations(t, dsn, schema, filepath.Join("..", "..", "migrations"))
}

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/migrations"
)

// Tracker records which migrations have been applied for one scope.
// CoreTracker covers the core schema; the plugin runtime provides a tracker
// backed by plugin_migrations.
type Tracker interface {
	// Ensure creates the tracking table if needed (runs outside the lock tx).
	Ensure(ctx context.Context, q *gorm.DB) error
	Applied(ctx context.Context, q *gorm.DB) (map[string]string, error) // id -> checksum
	Record(ctx context.Context, tx *gorm.DB, id, checksum string) error
}

// MigrateOptions controls one migration run.
type MigrateOptions struct {
	// LockKey is the pg_advisory_lock key serializing runs across nodes.
	LockKey int64
	// SearchPath, when set, is applied with SET LOCAL before each file.
	// Privilege separation is done by the connection's login role, never by
	// SET ROLE (a migration could RESET it).
	SearchPath string
	// RerunChanged re-runs an applied file whose content changed and records
	// the new checksum, instead of refusing it. Plugin migrations must be
	// idempotent, so a corrected script converges; core migrations keep the
	// strict rule.
	RerunChanged bool
}

// Migrate applies *.sql files from fsys in lexical order. Each file runs in
// its own transaction; a file whose checksum differs from the recorded one is
// an error (applied migrations are immutable). Returns applied file names.
//
// A session advisory lock on one connection serializes runs across nodes;
// the files' transactions run on that connection.
func Migrate(ctx context.Context, db *DB, fsys fs.FS, tr Tracker, opt MigrateOptions) (done []string, err error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	err = db.WithConn(ctx, func(conn *gorm.DB) error {
		if err := conn.Exec("SELECT pg_advisory_lock(?)", opt.LockKey).Error; err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		defer conn.WithContext(context.WithoutCancel(ctx)).Exec("SELECT pg_advisory_unlock(?)", opt.LockKey)
		if err := tr.Ensure(ctx, conn); err != nil {
			return err
		}
		applied, err := tr.Applied(ctx, conn)
		if err != nil {
			return err
		}
		for _, name := range names {
			ran, err := migrateFile(ctx, conn, fsys, name, applied, tr, opt)
			if err != nil {
				return err
			}
			if ran {
				done = append(done, name)
			}
		}
		return nil
	})
	return done, err
}

// migrateFile runs file name unless it is applied with the same checksum,
// and records it, in one transaction on conn.
func migrateFile(ctx context.Context, conn *gorm.DB, fsys fs.FS, name string, applied map[string]string, tr Tracker, opt MigrateOptions) (bool, error) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return false, err
	}
	checksum := MigrationChecksum(body)
	if prev, ok := applied[name]; ok {
		if prev == checksum {
			return false, nil
		}
		if !opt.RerunChanged {
			return false, fmt.Errorf("migration %s was modified after being applied", name)
		}
	}
	sqlBody := string(body)
	if core, ok := tr.(interface {
		migrationSQL(string, string, string) string
	}); ok {
		sqlBody = core.migrationSQL(name, checksum, sqlBody)
	}
	return true, conn.Transaction(func(tx *gorm.DB) error {
		if opt.SearchPath != "" {
			if err := tx.Exec("SET LOCAL search_path TO " + QuoteIdent(opt.SearchPath)).Error; err != nil {
				return err
			}
		}
		// The file goes to the driver as written, past GORM's placeholder
		// parsing ("?", "@name"): without arguments pgx sends it with the
		// simple protocol, which runs the several statements of a file.
		// A file's HINT (how to fix what it refused) ends up in the
		// start-up log with the message.
		if _, err := tx.Statement.ConnPool.ExecContext(ctx, sqlBody); err != nil {
			if hint := Hint(err); hint != "" {
				return fmt.Errorf("migration %s: %w; hint: %s", name, err, hint)
			}
			return fmt.Errorf("migration %s: %w", name, err)
		}
		return tr.Record(ctx, tx, name, checksum)
	})
}

// CoreTracker tracks core migrations in public.schema_migrations.
type CoreTracker struct{}

func (CoreTracker) Ensure(ctx context.Context, q *gorm.DB) error {
	return G(ctx, q).Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		id varchar(200) PRIMARY KEY,
		checksum varchar(64) NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`).Error
}

func (CoreTracker) Applied(ctx context.Context, q *gorm.DB) (map[string]string, error) {
	var rows []struct{ ID, Checksum string }
	if err := G(ctx, q).Table("schema_migrations").Select("id, checksum").Scan(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.ID] = r.Checksum
	}
	return supersedeApplied(renameApplied(m))
}

// MigrationChecksum is the checksum schema_migrations records for a
// migration file's content.
func MigrationChecksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// supersedeApplied reports a core migration applied with an earlier content
// that a development build carried (migrations.SupersededBuilds) with its
// current checksum: a database holding one is not refused as modified, and
// does not run it again.
func supersedeApplied(m map[string]string) (map[string]string, error) {
	builds, err := migrations.SupersededBuilds()
	if err != nil {
		return nil, err
	}
	for _, files := range builds {
		for id, body := range files {
			if m[id] != MigrationChecksum(body) {
				continue
			}
			cur, err := migrations.FS.ReadFile(id)
			if err != nil {
				return nil, err
			}
			m[id] = MigrationChecksum(cur)
		}
	}
	return m, nil
}

// renamedCoreMigrations maps the id of a core migration renamed after a build
// carrying the old name could have applied it to its current id. The file
// content is unchanged, so the recorded checksum still matches.
var renamedCoreMigrations = map[string]string{
	// Merged as a second 0073 next to 0073_api_key_group_loose.sql
	// (feat/next-platform 6094f565), renumbered before any release.
	"0073_failure_policy_split_auto_disable_any_position.sql": "0074_failure_policy_split_auto_disable_any_position.sql",
}

// renameApplied reports applied migrations under their current ids, so a
// database that applied a renamed file neither runs it again nor holds an id
// the core does not know.
func renameApplied(m map[string]string) map[string]string {
	for old, cur := range renamedCoreMigrations {
		sum, ok := m[old]
		if !ok {
			continue
		}
		delete(m, old)
		if _, done := m[cur]; !done {
			m[cur] = sum
		}
	}
	return m
}

func (CoreTracker) Record(ctx context.Context, tx *gorm.DB, id, checksum string) error {
	return G(ctx, tx).Create(&appliedMigration{ID: id, Checksum: checksum}).Error
}

// appliedMigration is a schema_migrations row; applied_at is the default.
type appliedMigration struct {
	ID       string `gorm:"column:id"`
	Checksum string `gorm:"column:checksum"`
}

func (appliedMigration) TableName() string { return "schema_migrations" }

func (CoreTracker) migrationSQL(id, checksum, body string) string {
	if id == "0046_proxy_real_ip.sql" && checksum == "1b0719bb3a07caa74ca6ad5d92e8c605e9c4f0ebea3097b96a1226ef99c4897f" {
		return strings.ReplaceAll(body, "ALTER TABLE proxies ADD COLUMN ", "ALTER TABLE proxies ADD COLUMN IF NOT EXISTS ")
	}
	return body
}

// CoreMigrationLockKey serializes core migrations across nodes.
const CoreMigrationLockKey int64 = 0x5375623241504931 // "Sub2API1"

// PluginMigrationLockKey derives a stable advisory lock key for a plugin.
func PluginMigrationLockKey(pluginKey string) int64 {
	sum := sha256.Sum256([]byte("plugin-migrations:" + strings.ToLower(pluginKey)))
	var k int64
	for i := 0; i < 8; i++ {
		k = k<<8 | int64(sum[i])
	}
	return k
}

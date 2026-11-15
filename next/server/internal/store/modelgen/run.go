package modelgen

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/migrations"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// FromMigrations creates a scratch database on the server of adminDSN (a
// role allowed to CREATE DATABASE), applies every core migration with the
// store's own migrator, generates the models from it and drops it.
func FromMigrations(ctx context.Context, adminDSN string, cfg Config) (map[string][]byte, error) {
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer admin.Close(context.WithoutCancel(ctx))

	name := fmt.Sprintf("modelgen_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return nil, err
	}
	defer admin.Exec(context.WithoutCancel(ctx), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") //nolint:errcheck

	u, err := url.Parse(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + name
	db, err := store.Open(ctx, u.String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := store.Migrate(ctx, db, migrations.FS, store.CoreTracker{},
		store.MigrateOptions{LockKey: store.CoreMigrationLockKey}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return Generate(ctx, db.Pool, cfg)
}

// Write replaces the generated files in dir with files, removing generated
// files that no longer have a table and leaving hand-written ones alone.
func Write(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old, err := ReadGenerated(dir)
	if err != nil {
		return err
	}
	for name := range old {
		if _, keep := files[name]; !keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadGenerated returns the generated .go files in dir by name.
func ReadGenerated(dir string) (map[string][]byte, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if bytes.HasPrefix(src, []byte(Header)) {
			files[filepath.Base(p)] = src
		}
	}
	return files, nil
}

// Package pgdbtool is the development side of package pgdb: it runs a
// plugin's migrations/*.sql (tests, sub2api-plugin) and compares them with
// the plugin's GORM models (Drift, behind sub2api-plugin db diff and check).
// Plugins never import it at run time.
package pgdbtool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Migration is one versioned migrations/*.sql file of a plugin.
type Migration struct {
	Name string // file name, e.g. 0001_init.sql
	Path string
}

// ReadMigrations lists the *.sql files of dirs, merged and sorted by file
// name: the order the host runs them in (ARCHITECTURE 5.6). A name in a
// later dir replaces the same name in an earlier one.
func ReadMigrations(dirs ...string) ([]Migration, error) {
	byName := map[string]string{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			return nil, fmt.Errorf("pgdb: read migrations %s: %w", d, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
				byName[e.Name()] = filepath.Join(d, e.Name())
			}
		}
	}
	out := make([]Migration, 0, len(byName))
	for name, path := range byName {
		out = append(out, Migration{Name: name, Path: path})
	}
	slices.SortFunc(out, func(a, b Migration) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ApplyMigrations runs each migration in its own transaction with
// search_path set to schema, like the host migration runner.
func ApplyMigrations(ctx context.Context, db *gorm.DB, schema string, migrations []Migration) error {
	for _, m := range migrations {
		text, err := os.ReadFile(m.Path)
		if err != nil {
			return fmt.Errorf("pgdb: read %s: %w", m.Name, err)
		}
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("SET LOCAL search_path TO ?", clause.Table{Name: schema}).Error; err != nil {
				return err
			}
			// No arguments: the driver sends the file as one simple query,
			// so it may hold several statements.
			return tx.Exec(string(text)).Error
		})
		if err != nil {
			return fmt.Errorf("pgdb: migration %s: %w", m.Name, err)
		}
	}
	return nil
}

// CreateSchema creates schema.
func CreateSchema(ctx context.Context, db *gorm.DB, schema string) error {
	return db.WithContext(ctx).Exec("CREATE SCHEMA ?", clause.Table{Name: schema}).Error
}

// DropSchema drops schema and everything in it, if it exists.
func DropSchema(ctx context.Context, db *gorm.DB, schema string) error {
	return db.WithContext(ctx).Exec("DROP SCHEMA IF EXISTS ? CASCADE", clause.Table{Name: schema}).Error
}

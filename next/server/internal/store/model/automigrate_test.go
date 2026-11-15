package model_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The SQL migrations are the only schema source; GORM must never create or
// alter tables from the models.
func TestNoAutoMigrate(t *testing.T) {
	schemaCalls := regexp.MustCompile(`\.(AutoMigrate|Migrator)\(`)
	root := filepath.Join("..", "..", "..") // next/server
	_, err := os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "module root not found")

	var hits []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (n == "node_modules" || n == "testdata" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if schemaCalls.Match(src) {
			hits = append(hits, p)
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, hits, "GORM schema changes found; change the schema with a migration in internal/migrations")
}

package pgdb_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pgxModule is spelt in two pieces so that a search of the sources for pgx
// imports does not find this file.
const pgxModule = "github.com/jackc/" + "pgx"

// No Go file of the SDK, the plugins, the gateway or the tools imports pgx,
// test files included (CONTRACTS §2): plugins speak GORM, and the driver's
// configuration and array type come from runtime-contract/gormpg, the one
// package outside the core that holds pgx (the core's own rule is
// gormguard's [pgx-import]). The sibling directories are checked when the
// SDK sits in the repository.
func TestNoPgxImports(t *testing.T) {
	next := filepath.Join("..", "..")
	seen := 0
	for _, dir := range []string{"sdk", "plugins", "gateway", "tools"} {
		root := filepath.Join(next, dir)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && (d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == "testdata"):
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go"):
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			seen++
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == pgxModule || strings.HasPrefix(p, pgxModule+"/") {
					t.Errorf("%s imports %s: use GORM, and runtime-contract/gormpg for what needs the driver", path, p)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen == 0 {
		t.Fatal("no Go file found")
	}
}

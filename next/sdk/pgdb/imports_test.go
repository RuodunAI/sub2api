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

// pgxDir is the one package outside the core that imports pgx: the driver's
// configuration (where the dial hook of strict network mode's egress tunnel
// goes) and the array type come from there for the core, the gateway and
// the SDK alike.
const pgxDir = "runtime-contract/gormpg/"

// No Go file of the SDK, the plugins, the gateway, the tools, the end-to-end
// tests, protocol-codec or runtime-contract imports pgx but those of pgxDir,
// test files, testdata and overlays included (CONTRACTS §2, "插件数据库"):
// plugins speak GORM through Host.DB. The core has its own rule
// ([pgx-import] in internal/store/gormguard). The sibling directories are
// checked when the SDK sits in the repository; alone, the SDK is checked by
// itself.
func TestNoPgxImports(t *testing.T) {
	next, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen, roots := 0, 0
	for _, dir := range []string{"sdk", "plugins", "gateway", "tools", "e2e", "protocol-codec", "runtime-contract"} {
		root := filepath.Join(next, dir)
		if _, err := os.Stat(root); err != nil {
			continue
		}
		roots++
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && (d.Name() == "node_modules" || d.Name() == "vendor"):
				return filepath.SkipDir
			case d.IsDir() || !strings.HasSuffix(path, ".go"):
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(next, path)
			rel = filepath.ToSlash(rel)
			seen++
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if (p == pgxModule || strings.HasPrefix(p, pgxModule+"/")) && !strings.HasPrefix(rel, pgxDir) {
					t.Errorf("%s imports %s: use the *gorm.DB of Host.DB (sdk/pgdb), and %s for what needs the driver", rel, p, pgxDir)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	switch {
	case seen == 0:
		t.Fatal("no Go file found")
	case roots > 1 && seen < 100:
		t.Fatalf("only %d Go files under next: the walk missed the tree", seen)
	}
}

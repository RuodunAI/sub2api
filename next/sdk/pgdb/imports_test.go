package pgdb_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pgxFile is the one Go file of the SDK and the plugins that imports pgx
// (CONTRACTS §2, "插件数据库"): the plugins speak GORM through Host.DB.
const pgxFile = "sdk/pgdb/pgdb.go"

// Every Go file under next/sdk and next/plugins, test files, testdata and
// overlays included, imports no pgx package but pgxFile. The server has
// its own rule ([pgx-import] in internal/store/gormguard).
func TestOnlyPgdbImportsPgx(t *testing.T) {
	next, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{"sdk"}
	if _, err := os.Stat(filepath.Join(next, "plugins")); err == nil {
		roots = append(roots, "plugins")
	} else {
		t.Log("no next/plugins next to the SDK: checking the SDK alone")
	}
	fset := token.NewFileSet()
	seen := 0
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(next, root), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
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
				if strings.HasPrefix(p, "github.com/jackc/pgx") && rel != pgxFile {
					t.Errorf("%s imports %s: only %s may import pgx; use the *gorm.DB of Host.DB (sdk/pgdb)", rel, p, pgxFile)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen < 100 {
		t.Fatalf("only %d Go files under %v: the walk missed the tree", seen, roots)
	}
}

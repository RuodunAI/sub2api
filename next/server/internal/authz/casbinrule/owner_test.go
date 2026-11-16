package casbinrule

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// literalOwners may spell casbin_rule in a string literal: this package,
// the one reader and writer, and the generated model of the table.
var literalOwners = []string{"internal/authz/casbinrule/", "internal/store/model/"}

// tableWord is the table's name as a word (not 0100_casbin_rule.sql's).
var tableWord = regexp.MustCompile(`\b` + Table + `\b`)

// No production Go file but the owners names casbin_rule in a string
// literal, so that no statement on the table, nor its mapping, is built
// anywhere else (casbinx names the table through Table). Comments and
// test files are free to.
func TestOnlyThisPackageNamesTheTable(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		for _, o := range literalOwners {
			if strings.HasPrefix(rel, o) {
				return nil
			}
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && tableWord.MatchString(lit.Value) {
				t.Errorf("%s: names %s; read and write it through package casbinrule", fset.Position(lit.Pos()), Table)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

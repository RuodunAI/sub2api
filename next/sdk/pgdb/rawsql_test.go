package pgdb_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// rawSQLFiles lists the files, relative to next/, that may run an SQL
// statement written out in Raw or Exec, with the reason. Everywhere else a
// statement is built by GORM from the models (CONTRACTS §2, "插件数据库"):
// Raw and Exec only combine built queries (sqlCombinator).
var rawSQLFiles = map[string]string{
	"sdk/pgdb/pgdbtool/catalog.go":  "schema drift check: reads pg_catalog (columns, constraints, indexes, dependent views) and compares two expressions in the server's own casts",
	"sdk/pgdb/pgdbtool/migrator.go": "schema drift check: reads a column default from pg_attrdef and normalizes one through a temporary table under a savepoint (DDL)",
	"sdk/pgdb/pgdbtool/drift.go":    "migration replay: SET LOCAL search_path / ROLE, RESET ROLE and the catalog read of the role's grants",
	"sdk/pgdb/pgdbtool/migrate.go":  "migration runner: CREATE SCHEMA / DROP SCHEMA and SET LOCAL search_path (DDL)",
	"sdk/pgdb/query.go":             "NextIDs (nextval of a serial over generate_series) and LockTable (LOCK TABLE): no data statements",
}

// dynamicSQLFiles lists the files, relative to next/, whose SQL text (the
// first argument of Raw, Exec, Where, Select, Order, Joins, Table,
// gorm.Expr, clause.Expr ...) may be built at run time, with the reason.
// Everywhere else it is a constant: values go in as ? parameters, and
// identifiers chosen by a request through a fixed whitelist into
// clause.Column, clause.Table or clause.OrderByColumn, so that no input
// ever becomes SQL text.
var dynamicSQLFiles = map[string]string{
	"sdk/pgdb/pgdbtool/catalog.go":  "compares two default expressions read from pg_catalog (never input) cast to a type format_type printed",
	"sdk/pgdb/pgdbtool/migrator.go": "the CREATE TEMPORARY TABLE of a default probe: the type and default read from pg_catalog / the model, never input",
	"sdk/pgdb/pgdbtool/drift.go":    "replays the recorded statements and the migration files of the plugin being packed",
	"sdk/pgdb/pgdbtool/migrate.go":  "runs the migration files of the plugin being migrated",
}

// sqlTextMethods are the methods of *gorm.DB whose first argument, when it
// is a string, is SQL text.
var sqlTextMethods = map[string]bool{
	"Raw": true, "Exec": true, "Where": true, "Not": true, "Or": true, "Having": true, "Select": true,
	"Omit": false, "Order": true, "Group": true, "Joins": true, "InnerJoins": true, "Table": true, "Distinct": true,
}

// sqlCombinator matches Raw and Exec text that only combines what GORM
// built, passed as parameters: SELECT EXISTS (?), (?) UNION ALL (?),
// INSERT INTO ? ? ? (pgdb.InsertSelect).
var sqlCombinator = regexp.MustCompile(`(?i)^(\s|[?(),]|\b(SELECT|EXISTS|UNION|ALL|INTERSECT|EXCEPT|INSERT|INTO)\b)*$`)

const (
	gormPkg   = "gorm.io/gorm"
	clausePkg = "gorm.io/gorm/clause"
	pgdbPkg   = "github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// TestNoRawSQL keeps the SDK's and the plugins' statements built by GORM:
// no Raw or Exec of a statement written out outside rawSQLFiles ([raw-sql]),
// and no SQL text built at run time outside dynamicSQLFiles ([sql-const]).
// Test files are not checked. Like TestNoPgxImports, the plugins are
// checked when the SDK sits in the repository (next/go.work); alone, the
// SDK is checked by itself.
func TestNoRawSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks every plugin")
	}
	next, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir, patterns := filepath.Join(next, "sdk"), []string{"./..."}
	if _, err := os.Stat(filepath.Join(next, "go.work")); err == nil {
		dir, patterns = next, workModules(t, next, "sdk", "plugins")
	}
	var targets []listed
	files := map[string]bool{}
	for _, p := range goList(t, dir, patterns...) {
		for _, name := range p.GoFiles {
			files[relTo(next, filepath.Join(p.Dir, name))] = true
		}
		if slices.Contains(p.Deps, gormPkg) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		t.Fatal("no package uses GORM: the scan missed the tree")
	}
	paths := make([]string, len(targets))
	for i, p := range targets {
		paths[i] = p.ImportPath
	}
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, dir, append([]string{"-export", "-deps"}, paths...)...))
	found := map[string]map[string]bool{"raw-sql": {}, "sql-const": {}}
	for _, p := range targets {
		var parsed []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			parsed = append(parsed, f)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{}}
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, parsed, info); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, f := range parsed {
			for _, v := range sqlFindings(info, f) {
				pos := fset.Position(v.at)
				rel := relTo(next, pos.Filename)
				found[v.rule][rel] = true
				allowed := rawSQLFiles
				if v.rule == "sql-const" {
					allowed = dynamicSQLFiles
				}
				if _, ok := allowed[rel]; !ok {
					t.Errorf("%s:%d: [%s] %s", rel, pos.Line, v.rule, v.msg)
				}
			}
		}
	}
	for rule, allowed := range map[string]map[string]string{"raw-sql": rawSQLFiles, "sql-const": dynamicSQLFiles} {
		for rel := range allowed {
			switch {
			case dir != next && !strings.HasPrefix(rel, "sdk/"):
			case !files[rel]:
				t.Errorf("%s is listed for [%s] but is no Go file of the tree; drop its entry", rel, rule)
			case !found[rule][rel]:
				t.Errorf("%s is listed for [%s] but needs no exception; drop its entry", rel, rule)
			}
		}
	}
}

type sqlFinding struct {
	at        token.Pos
	rule, msg string
}

// sqlFindings walks f for SQL text: the first argument of the
// sqlTextMethods of *gorm.DB, of gorm.Expr, and the SQL of clause.Expr and
// clause.NamedExpr literals.
func sqlFindings(info *types.Info, f *ast.File) []sqlFinding {
	var out []sqlFinding
	text := func(e ast.Expr, what string, raw bool) {
		tv, ok := info.Types[e]
		if !ok || !isString(tv.Type) {
			return // a struct, a map, a clause: no SQL text
		}
		if tv.Value == nil && !isSQLText(info, e) {
			out = append(out, sqlFinding{e.Pos(), "sql-const", what + " takes SQL text built at run time: write it as a constant, pass values as ? parameters and identifiers through a whitelist as clause.Column / clause.Table / clause.OrderByColumn"})
			return
		}
		if raw && !sqlCombinator.MatchString(constant.StringVal(tv.Value)) {
			out = append(out, sqlFinding{e.Pos(), "raw-sql", what + " runs an SQL statement written out: build it with GORM (Model/Table, Where, Select, Create, Updates, Clauses ...)"})
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if tv, ok := info.Types[n.Fun]; ok && tv.IsType() && isSQLType(tv.Type) && len(n.Args) == 1 {
				if info.Types[n.Args[0]].Value == nil && !isSQLType(info.Types[n.Args[0]].Type) {
					out = append(out, sqlFinding{n.Pos(), "sql-const", "pgdb.SQL made of a value built at run time: SQL text is a constant"})
				}
				return true
			}
			sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
			if !ok || len(n.Args) == 0 {
				return true
			}
			if s := info.Selections[sel]; s != nil && sqlTextMethods[sel.Sel.Name] && isGormDB(s.Recv()) {
				text(n.Args[0], sel.Sel.Name, sel.Sel.Name == "Raw" || sel.Sel.Name == "Exec")
			} else if obj, ok := info.Uses[sel.Sel].(*types.Func); ok && obj.Pkg() != nil && obj.Pkg().Path() == gormPkg && obj.Name() == "Expr" {
				text(n.Args[0], "gorm.Expr", false)
			}
		case *ast.CompositeLit:
			named, ok := types.Unalias(info.Types[n].Type).(*types.Named)
			if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != clausePkg ||
				(named.Obj().Name() != "Expr" && named.Obj().Name() != "NamedExpr") {
				return true
			}
			for i, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "SQL" {
						text(kv.Value, "clause."+named.Obj().Name(), false)
					}
				} else if i == 0 {
					text(el, "clause."+named.Obj().Name(), false)
				}
			}
		}
		return true
	})
	return out
}

// isSQLType reports whether t is pgdb.SQL, the SQL text helpers pass on.
func isSQLType(t types.Type) bool {
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pgdbPkg && named.Obj().Name() == "SQL"
}

// isSQLText reports whether e is string(x) of a pgdb.SQL x: text a helper
// was handed as a constant (pgdb.SQL only takes constants), passed on.
func isSQLText(info *types.Info, e ast.Expr) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	tv := info.Types[call.Fun]
	return tv.IsType() && isString(tv.Type) && isSQLType(info.Types[call.Args[0]].Type)
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// isGormDB reports whether t is gorm.DB or a pointer to it.
func isGormDB(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == gormPkg && named.Obj().Name() == "DB"
}

func relTo(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// --- loading (as internal/store/gormguard in the core) -------------------

// workModules returns a ./dir/... pattern for each module of next/go.work
// under one of the roots: a pattern spans one module only.
func workModules(t *testing.T, next string, roots ...string) []string {
	t.Helper()
	cmd := exec.Command("go", "work", "edit", "-json")
	cmd.Dir = next
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go work edit -json: %v", err)
	}
	var work struct{ Use []struct{ DiskPath string } }
	if err := json.Unmarshal(out, &work); err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, u := range work.Use {
		p := filepath.ToSlash(filepath.Clean(u.DiskPath))
		if slices.ContainsFunc(roots, func(r string) bool { return p == r || strings.HasPrefix(p, r+"/") }) {
			patterns = append(patterns, "./"+p+"/...")
		}
	}
	return patterns
}

type listed struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Deps       []string
	Export     string
	Error      *struct{ Err string }
}

// goList runs go list -json in dir. The go command is the one that builds
// the tests, so its export data matches this go/types.
func goList(t *testing.T, dir string, args ...string) []listed {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-json=ImportPath,Dir,GoFiles,Deps,Export,Error"}, args...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v\n%s", args, err, stderr.String())
	}
	var pkgs []listed
	for dec := json.NewDecoder(bytes.NewReader(out)); ; {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs
		} else if err != nil {
			t.Fatal(err)
		}
		if p.Error != nil {
			t.Fatalf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs = append(pkgs, p)
	}
}

// exportImporter imports packages from the export data go list -export
// produced, so only the checked packages are type-checked from source.
func exportImporter(fset *token.FileSet, pkgs []listed) types.Importer {
	files := map[string]string{}
	for _, p := range pkgs {
		if p.Export != "" {
			files[p.ImportPath] = p.Export
		}
	}
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
}

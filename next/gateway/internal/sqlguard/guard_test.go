package sqlguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	gatewayModule = "github.com/Sub2API-Devs/sup2api/next/gateway"
	gormPkg       = "gorm.io/gorm"
	clausePkg     = "gorm.io/gorm/clause"
)

// rawAllowed lists the functions, as file (relative to the module) and
// function name, that may run Raw or Exec ([raw-sql]), with the reason:
// what GORM's builder cannot express. A function literal belongs to the
// function it is written in.
var rawAllowed = map[string]string{
	"internal/control/store.go:execLocked":  "DDL: the shell schema script behind its transaction-scoped advisory lock, one simple-protocol statement",
	"internal/control/store.go:Heartbeat":   "data-modifying CTE: the conditional UPDATE and the identity check in one round trip per heartbeat; GORM cannot put an UPDATE in a WITH",
	"internal/control/db.go:existsQuery":    "combinator: SELECT EXISTS (q1), EXISTS (q2) ... over builder queries, in one round trip (the core's store.ExistsQuery)",
	"internal/control/db.go:relationExists": "catalog: to_regclass, whether the core's schema has a table",
}

// constAllowed lists the functions that may hand GORM SQL text built at run
// time ([sql-const]), with the reason: it is built from constants only.
var constAllowed = map[string]string{
	"internal/control/db.go:existsQuery":   "one constant EXISTS (?) per query; only the count varies",
	"internal/control/store.go:execLocked": "the embedded schema.sql (go:embed, so a variable) after the constant schemaLock",
}

// sqlMethods are the methods of *gorm.DB whose first argument, a string, is
// SQL text ([sql-const]); Raw and Exec are [raw-sql] as well.
var sqlMethods = map[string]bool{
	"Raw": true, "Exec": true, "Where": true, "Not": true, "Or": true, "Having": true, "Select": true,
	"Distinct": true, "Order": true, "Group": true, "Joins": true, "Table": true, "Pluck": true,
}

// --- loading ---------------------------------------------------------------

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

// moduleRoot is the gateway module's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", gatewayModule).Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
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

func typeCheck(fset *token.FileSet, imp types.Importer, path string, files []*ast.File) (*types.Info, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	_, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	return info, err
}

// --- the module --------------------------------------------------------------

// TestGatewaySQLRules checks every non-test file of the module's packages
// that depend on GORM.
func TestGatewaySQLRules(t *testing.T) {
	root := moduleRoot(t)
	var targets []listed
	for _, p := range goList(t, root, "./...") {
		if slices.Contains(p.Deps, gormPkg) {
			targets = append(targets, p)
		}
	}
	if !slices.ContainsFunc(targets, func(p listed) bool { return p.ImportPath == gatewayModule+"/internal/control" }) {
		t.Fatal("internal/control was not among the GORM users: the scan did not see the module")
	}
	paths := make([]string, len(targets))
	for i, p := range targets {
		paths[i] = p.ImportPath
	}
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, append([]string{"-export", "-deps"}, paths...)...))
	c := newChecker(gatewayModule, rawAllowed, constAllowed)
	for _, p := range targets {
		files := parseFiles(t, fset, p.Dir, p.GoFiles)
		info, err := typeCheck(fset, imp, p.ImportPath, files)
		if err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		c.check(fset, p.ImportPath, files, info)
	}
	for _, finding := range c.findings() {
		t.Error(finding)
	}
}

func parseFiles(t *testing.T, fset *token.FileSet, dir string, names []string) []*ast.File {
	t.Helper()
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// --- the rules -----------------------------------------------------------------

type checker struct {
	module         string
	raw, constants map[string]string
	used           map[string]bool
	out            []string
}

func newChecker(module string, raw, constants map[string]string) *checker {
	return &checker{module: module, raw: raw, constants: constants, used: map[string]bool{}}
}

// findings returns what check reported, and every allowlist entry that
// allowed nothing: an entry outlives its statement only by mistake.
func (c *checker) findings() []string {
	out := slices.Clone(c.out)
	for _, list := range []map[string]string{c.raw, c.constants} {
		var stale []string
		for key := range list {
			if !c.used[key] {
				stale = append(stale, key)
			}
		}
		sort.Strings(stale)
		for _, key := range stale {
			out = append(out, key+" is allowlisted but has no such statement; drop its entry")
		}
	}
	return out
}

// check reports the findings of the non-test files of package pkgPath.
func (c *checker) check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) {
	rel := strings.TrimPrefix(strings.TrimPrefix(pkgPath, c.module), "/")
	for _, f := range files {
		name := filepath.Base(fset.Position(f.Package).Filename)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file := filepath.ToSlash(filepath.Join(rel, name))
		for _, decl := range f.Decls {
			fn := ""
			if d, ok := decl.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			key := file + ":" + fn
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					c.call(fset, info, key, n)
				case *ast.CompositeLit:
					c.literal(fset, info, key, n)
				}
				return true
			})
		}
	}
}

// call checks a call of a *gorm.DB method or of gorm.Expr.
func (c *checker) call(fset *token.FileSet, info *types.Info, key string, call *ast.CallExpr) {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return
	}
	method := sel.Sel.Name
	if s := info.Selections[sel]; s != nil && s.Kind() == types.MethodVal && isGormDB(s.Recv()) && sqlMethods[method] {
		if method == "Raw" || method == "Exec" {
			c.report(fset, call.Pos(), key, c.raw, "[raw-sql] (*gorm.DB)."+method+": build the statement with GORM's models and builder, or allowlist the function with the reason GORM cannot express it")
		}
		c.sqlText(fset, info, key, "(*gorm.DB)."+method, call.Args[0])
		return
	}
	if f, ok := info.Uses[sel.Sel].(*types.Func); ok && f.Pkg() != nil && f.Pkg().Path() == gormPkg && f.Name() == "Expr" {
		c.sqlText(fset, info, key, "gorm.Expr", call.Args[0])
	}
}

// literal checks the SQL of a clause.Expr or clause.NamedExpr literal.
func (c *checker) literal(fset *token.FileSet, info *types.Info, key string, lit *ast.CompositeLit) {
	named, ok := types.Unalias(info.Types[lit].Type).(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != clausePkg || (named.Obj().Name() != "Expr" && named.Obj().Name() != "NamedExpr") {
		return
	}
	for i, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "SQL" {
				c.sqlText(fset, info, key, "clause."+named.Obj().Name(), kv.Value)
			}
		} else if i == 0 {
			c.sqlText(fset, info, key, "clause."+named.Obj().Name(), el)
		}
	}
}

// sqlText reports SQL text that is not a constant: a string argument without
// a constant value, or a []string that is not a literal of constants.
func (c *checker) sqlText(fset *token.FileSet, info *types.Info, key, where string, arg ast.Expr) {
	const msg = "[sql-const] SQL text of %s is not a constant: values go in as ? parameters and identifiers from input through a fixed map (clause.Column, clause.OrderByColumn)"
	tv := info.Types[arg]
	if tv.Type == nil {
		return
	}
	switch t := tv.Type.Underlying().(type) {
	case *types.Basic:
		if t.Info()&types.IsString != 0 && tv.Value == nil {
			c.report(fset, arg.Pos(), key, c.constants, fmt.Sprintf(msg, where))
		}
	case *types.Slice:
		if b, ok := t.Elem().Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
			return
		}
		lit, ok := ast.Unparen(arg).(*ast.CompositeLit)
		if !ok {
			c.report(fset, arg.Pos(), key, c.constants, fmt.Sprintf(msg, where))
			return
		}
		for _, el := range lit.Elts {
			if info.Types[el].Value == nil {
				c.report(fset, el.Pos(), key, c.constants, fmt.Sprintf(msg, where))
			}
		}
	}
}

// report records a finding at pos unless key's function is in allowed.
func (c *checker) report(fset *token.FileSet, pos token.Pos, key string, allowed map[string]string, msg string) {
	if _, ok := allowed[key]; ok {
		c.used[key] = true
		return
	}
	c.out = append(c.out, fmt.Sprintf("%s (%s): %s", fset.Position(pos), key, msg))
}

// isGormDB reports *gorm.DB (or gorm.DB).
func isGormDB(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == gormPkg && n.Obj().Name() == "DB"
}

// --- the rules, on a fixture -----------------------------------------------------

const fixture = `package fixture

import (
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const table = "t"

type pipe struct{}

func (pipe) Exec(string) error { return nil }

func ok(db *gorm.DB, id int64, p pipe) {
	db.Table(table+" x").Where("id = ? AND "+table+".a", id).Select([]string{"a", "b"}).Order("id").Find(&struct{}{})
	db.Where(map[string]any{"id": id}).Updates(map[string]any{"n": gorm.Expr("n + ?", 1)})
	db.Clauses(clause.Expr{SQL: "x"})
	_ = p.Exec("not gorm")
}

func raw(db *gorm.DB) {
	db.Exec("DELETE FROM t")
	db.Raw("SELECT 1")
}

func allowed(db *gorm.DB) { db.Raw("SELECT 1") }

func variable(db *gorm.DB, col, dir string, cols []string) {
	db.Order(col + " " + dir)
	db.Where(fmt.Sprintf("%s = ?", col), 1)
	db.Select(cols)
	db.Select([]string{"a", col})
	_ = gorm.Expr(col)
	_ = clause.Expr{SQL: col}
	_ = clause.NamedExpr{col, nil}
}
`

// TestRules runs the checker on the fixture: every finding it must and must
// not report, and a stale allowlist entry.
func TestRules(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, "-export", "-deps", gormPkg, clausePkg, "fmt"))
	f, err := parser.ParseFile(fset, "fixture.go", fixture, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info, err := typeCheck(fset, imp, "example.com/fixture", []*ast.File{f})
	if err != nil {
		t.Fatal(err)
	}
	c := newChecker("example.com", map[string]string{"fixture/fixture.go:allowed": "test", "fixture/fixture.go:gone": "test"}, nil)
	c.check(fset, "example.com/fixture", []*ast.File{f}, info)
	got := map[string]int{}
	for _, finding := range c.findings() {
		switch {
		case strings.Contains(finding, "[raw-sql]"):
			got["raw:"+finding[strings.Index(finding, "(")+1:strings.Index(finding, ")")]]++
		case strings.Contains(finding, "[sql-const]"):
			got["const:"+finding[strings.Index(finding, "(")+1:strings.Index(finding, ")")]]++
		case strings.Contains(finding, "has no such statement"):
			got["stale:"+strings.Fields(finding)[0]]++
		default:
			t.Errorf("unexpected finding %s", finding)
		}
	}
	want := map[string]int{
		"raw:fixture/fixture.go:raw":        2,
		"const:fixture/fixture.go:variable": 7,
		"stale:fixture/fixture.go:gone":     1,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("findings %v, want %v\n%s", got, want, strings.Join(c.findings(), "\n"))
	}
}

package gormguard

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
	"strings"
	"testing"
)

const (
	gormPkg   = "gorm.io/gorm"
	pgxPrefix = "github.com/jackc/pgx/v5"
)

// openAllowed lists the packages, relative to the module, that may call
// gorm.Open, with the reason.
var openAllowed = map[string]string{
	"internal/store":          "store.G's instance over the forwarding driver",
	"internal/store/gormtest": "the DryRun instance of the SQL snapshots; it never connects",
}

// migratedPackages lists the packages, relative to the module, whose
// statements are built with store.G: they must not go back to pgx Exec,
// Query or QueryRow. A package is added here in the batch that migrates it.
var migratedPackages = map[string]string{}

// Rule tags, at the start of every finding.
const (
	ruleTx    = "[gorm-tx]"
	ruleOpen  = "[gorm-open]"
	ruleSlice = "[gorm-slice]"
	rulePgx   = "[pgx-direct]"
)

// bannedMethods are the gorm (*DB) methods that would let GORM own a
// transaction or the schema.
var bannedMethods = map[string]bool{"Transaction": true, "Begin": true, "AutoMigrate": true, "Migrator": true}

// conditionMethods are the gorm (*DB) methods whose arguments become SQL
// parameters (for Find, First, Take, Last and Delete: the inline
// conditions).
var conditionMethods = map[string]bool{
	"Where": true, "Or": true, "Not": true, "Having": true, "Raw": true, "Exec": true, "Joins": true,
	"Find": true, "First": true, "Take": true, "Last": true, "Delete": true,
}

// pgxMethods are the direct statement calls of a pgx querier.
var pgxMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true}

type rules struct {
	module   string
	openAt   map[string]string
	migrated map[string]string
}

// check returns the findings of one type-checked package as
// "file:line:col: [rule] message".
func (r rules) check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) []string {
	rel := strings.TrimPrefix(pkgPath, r.module+"/")
	_, mayOpen := r.openAt[rel]
	_, migrated := r.migrated[rel]
	var out []string
	report := func(n ast.Node, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s: %s", fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				// Selectors, not calls: a method value (f := db.Begin) is
				// caught as well.
				fn := funcOf(info, n)
				switch {
				case fn == nil:
				case isGormMethod(fn) && bannedMethods[fn.Name()]:
					report(n, "%s gorm (*DB).%s: transactions start in store.DB.Tx and the schema comes from the SQL migrations", ruleTx, fn.Name())
				case isGormFunc(fn, "Open") && !mayOpen:
					report(n, "%s gorm.Open outside package store: take a handle from store.G", ruleOpen)
				case migrated && isPgxStatement(fn):
					report(n, "%s pgx %s in a migrated package: build the statement with store.G (Batch and CopyFrom stay on pgx)", rulePgx, fn.Name())
				}
			case *ast.CallExpr:
				sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn := funcOf(info, sel)
				if fn == nil || !(isGormMethod(fn) && conditionMethods[fn.Name()] || isGormFunc(fn, "Expr")) {
					return true
				}
				for _, arg := range n.Args {
					if t := info.TypeOf(arg); expandedSlice(t) {
						report(arg, "%s %s argument to gorm %s: GORM expands it into a list; pass store.Int64s or store.Strings", ruleSlice, t, fn.Name())
					}
				}
			}
			return true
		})
	}
	return out
}

// funcOf is the function or method a selector names, if any.
func funcOf(info *types.Info, sel *ast.SelectorExpr) *types.Func {
	if s := info.Selections[sel]; s != nil {
		fn, _ := s.Obj().(*types.Func)
		return fn
	}
	fn, _ := info.Uses[sel.Sel].(*types.Func) // a qualified identifier: gorm.Open
	return fn
}

func isNamed(t types.Type, pkg, name string) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

// isGormMethod reports a method of gorm.DB, including one promoted through
// an embedded *gorm.DB.
func isGormMethod(fn *types.Func) bool {
	recv := fn.Type().(*types.Signature).Recv()
	return recv != nil && isNamed(recv.Type(), gormPkg, "DB")
}

func isGormFunc(fn *types.Func, name string) bool {
	return fn.Pkg() != nil && fn.Pkg().Path() == gormPkg && fn.Name() == name && fn.Type().(*types.Signature).Recv() == nil
}

// isPgxStatement recognises Exec, Query and QueryRow by shape (a context
// first, a pgx or pgconn result first), so it covers *pgxpool.Pool, pgx.Tx,
// *pgx.Conn and store.Querier alike, and leaves out pgx.BatchResults (no
// context) and gorm's Exec (no context).
func isPgxStatement(fn *types.Func) bool {
	if !pgxMethods[fn.Name()] {
		return false
	}
	sig := fn.Type().(*types.Signature)
	if sig.Recv() == nil || sig.Params().Len() == 0 || sig.Results().Len() == 0 || !isNamed(sig.Params().At(0).Type(), "context", "Context") {
		return false
	}
	n, ok := types.Unalias(sig.Results().At(0).Type()).(*types.Named)
	return ok && n.Obj().Pkg() != nil && strings.HasPrefix(n.Obj().Pkg().Path(), pgxPrefix)
}

// expandedSlice reports a slice of int, int32, int64 or string (named or
// not) that is no driver.Valuer: GORM expands exactly those. []byte is a
// bytea value and store.Int64s and store.Strings are Valuers.
func expandedSlice(t types.Type) bool {
	if t == nil {
		return false
	}
	s, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	b, ok := s.Elem().Underlying().(*types.Basic)
	if !ok {
		return false
	}
	switch b.Kind() {
	case types.Int, types.Int32, types.Int64, types.String:
	default:
		return false
	}
	obj, _, _ := types.LookupFieldOrMethod(t, false, nil, "Value")
	_, valuer := obj.(*types.Func)
	return !valuer
}

// --- loading -------------------------------------------------------------

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

// moduleRoot returns the directory and path of the module under test.
func moduleRoot(t *testing.T) (dir, path string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}} {{.Path}}", "github.com/Sub2API-Devs/sup2api/next/server").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	dir, path, _ = strings.Cut(strings.TrimSpace(string(out)), " ")
	return dir, path
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

// --- the repository ------------------------------------------------------

// TestRepositoryFollowsGORMRules checks every non-test file of the packages
// the rules can apply to: those that depend on GORM, and the migrated ones.
func TestRepositoryFollowsGORMRules(t *testing.T) {
	root, module := moduleRoot(t)
	r := rules{module: module, openAt: openAllowed, migrated: migratedPackages}
	known := map[string]bool{}
	var targets []listed
	for _, p := range goList(t, root, "./...") {
		rel := strings.TrimPrefix(p.ImportPath, module+"/")
		known[rel] = true
		if _, migrated := r.migrated[rel]; migrated || slices.Contains(p.Deps, gormPkg) {
			targets = append(targets, p)
		}
	}
	for _, list := range []map[string]string{openAllowed, migratedPackages} {
		for rel := range list {
			if !known[rel] {
				t.Errorf("%s is listed but is no package of %s; drop its entry", rel, module)
			}
		}
	}
	if !slices.ContainsFunc(targets, func(p listed) bool { return p.ImportPath == module+"/internal/store/gormtest" }) {
		t.Fatal("internal/store/gormtest was not among the GORM users: the scan did not see the module")
	}
	paths := make([]string, len(targets))
	for i, p := range targets {
		paths[i] = p.ImportPath
	}
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, append([]string{"-export", "-deps"}, paths...)...))
	for _, p := range targets {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info, err := typeCheck(fset, imp, p.ImportPath, files)
		if err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		for _, finding := range r.check(fset, p.ImportPath, files, info) {
			t.Error(finding)
		}
	}
}

// --- the rules, on fixtures ----------------------------------------------

const fixtureModule = "github.com/Sub2API-Devs/sup2api/next/server"

const fixtureImports = `
import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)
`

const fixtureUses = `
var (
	_ context.Context
	_ pgx.Tx
	_ *pgxpool.Pool
	_ *gorm.DB
	_ store.Querier
)
`

func TestRules(t *testing.T) {
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, "-export", "-deps",
		gormPkg, pgxPrefix, pgxPrefix+"/pgxpool", fixtureModule+"/internal/store"))
	r := rules{
		module:   fixtureModule,
		openAt:   map[string]string{"internal/store": "test"},
		migrated: map[string]string{"internal/usage": "test"},
	}
	cases := []struct {
		name    string
		pkg     string // relative to the module
		imports string // extra imports
		src     string
		want    []string // the rule tags of the findings, in source order
	}{
		{
			name: "tx/banned calls, a method value and an embedded DB",
			pkg:  "internal/billing",
			src: `
func f(db *gorm.DB) {
	_ = db.Transaction(func(*gorm.DB) error { return nil })
	db.Begin()
	_ = db.AutoMigrate(&struct{}{})
	_ = db.Migrator()
	begin := db.Begin
	_ = begin
}

type repo struct{ *gorm.DB }

func g(r repo) { r.Begin() }
`,
			want: []string{ruleTx, ruleTx, ruleTx, ruleTx, ruleTx, ruleTx},
		},
		{
			name: "tx/queries, pgx transactions and look-alikes pass",
			pkg:  "internal/billing",
			src: `
type fake struct{}

func (fake) Begin()       {}
func (fake) Transaction() {}

func f(ctx context.Context, db *gorm.DB, pool *pgxpool.Pool, x fake) {
	var rows []struct{ ID int64 }
	db.Where("id = ?", 1).Find(&rows)
	_, _ = pool.Begin(ctx)
	x.Begin()
	x.Transaction()
}
`,
		},
		{
			name:    "open/outside store, through an alias and as a value",
			pkg:     "internal/usage",
			imports: `import g "gorm.io/gorm"`,
			src: `
func f(d gorm.Dialector) {
	_, _ = gorm.Open(d)
	_, _ = g.Open(d, &g.Config{})
	open := gorm.Open
	_ = open
}
`,
			want: []string{ruleOpen, ruleOpen, ruleOpen},
		},
		{
			name: "open/inside store",
			pkg:  "internal/store",
			src:  `func f(d gorm.Dialector) { _, _ = gorm.Open(d) }`,
		},
		{
			name: "open/other functions named Open",
			pkg:  "internal/usage",
			src: `
func Open() {}

func f(ctx context.Context) {
	Open()
	_, _ = store.Open(ctx, "")
}
`,
		},
		{
			name: "slice/every parameter position and type",
			pkg:  "internal/billing",
			src: `
type ids []int64

func f(db *gorm.DB, a []int64, b []string, c []int32, d []int, e ids) {
	var rows []struct{ ID int64 }
	db.Where("id = ANY(?)", a).Find(&rows)
	db.Where("name = ANY(?)", b)
	db.Or("x IN ?", c)
	db.Not("x IN ?", d)
	db.Having("x IN ?", e)
	db.Raw("SELECT ?", []int64{1})
	db.Exec("DELETE FROM t WHERE id = ANY(?)", a)
	db.Joins("JOIN t ON t.id = ANY(?)", a)
	db.Find(&rows, a)
	db.Delete(&rows, a)
	_ = gorm.Expr("x = ANY(?)", a)
	db.Where(a)
}
`,
			want: []string{ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice},
		},
		{
			name: "slice/array types, bytes, scalars and column lists pass",
			pkg:  "internal/billing",
			src: `
type builder struct{}

func (builder) Where(string, ...any) {}

func f(db *gorm.DB, a []int64, raw []byte, b builder) {
	var rows []struct{ ID int64 }
	db.Where("id = ANY(?)", store.Int64s(a)).Find(&rows)
	db.Where("id = ANY(?)", store.Int64s(nil))
	db.Where("name = ANY(?)", store.Strings{"x"})
	db.Where("blob = ?", raw)
	db.Where("id = ?", a[0])
	db.Where(map[string]any{"id": 1})
	db.Select([]string{"id", "name"}).Find(&rows)
	db.Pluck("id", &a)
	b.Where("x", []int64{1})
}
`,
		},
		{
			name: "pgx/direct statements in a migrated package",
			pkg:  "internal/usage",
			src: `
func f(ctx context.Context, q store.Querier, pool *pgxpool.Pool, tx pgx.Tx, conn *pgx.Conn) {
	_, _ = q.Exec(ctx, "x")
	_, _ = pool.Query(ctx, "x")
	_ = tx.QueryRow(ctx, "x")
	_, _ = conn.Exec(ctx, "x")
	exec := q.Exec
	_ = exec
}
`,
			want: []string{rulePgx, rulePgx, rulePgx, rulePgx, rulePgx},
		},
		{
			name: "pgx/batches, CopyFrom and gorm Exec pass in a migrated package",
			pkg:  "internal/usage",
			src: `
func f(ctx context.Context, tx pgx.Tx, db *gorm.DB) error {
	b := &pgx.Batch{}
	b.Queue("x")
	br := tx.SendBatch(ctx, b)
	_, _ = br.Exec()
	_ = br.QueryRow()
	_, _ = br.Query()
	_, _ = tx.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"a"}, pgx.CopyFromRows(nil))
	db.Exec("SELECT 1")
	return br.Close()
}
`,
		},
		{
			name: "pgx/direct statements outside the migrated packages",
			pkg:  "internal/billing",
			src: `
func f(ctx context.Context, q store.Querier, pool *pgxpool.Pool) {
	_, _ = q.Exec(ctx, "x")
	_, _ = pool.Query(ctx, "x")
}
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\n" + fixtureImports + c.imports + "\n" + c.src + fixtureUses
			f, err := parser.ParseFile(fset, c.name+".go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			path := fixtureModule + "/" + c.pkg
			info, err := typeCheck(fset, imp, path, []*ast.File{f})
			if err != nil {
				t.Fatalf("fixture does not type-check: %v", err)
			}
			findings := r.check(fset, path, []*ast.File{f}, info)
			var got []string
			for _, finding := range findings {
				got = append(got, strings.Fields(finding[strings.Index(finding, " [")+1:])[0])
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("rules %v, want %v\n%s", got, c.want, strings.Join(findings, "\n"))
			}
		})
	}
}

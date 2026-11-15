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
	clausePkg = "gorm.io/gorm/clause"
	pgxPrefix = "github.com/jackc/pgx/v5"
	// allowPgx starts the comment that keeps one pgx statement in a
	// migrated package: //gormguard:allow-pgx <reason>, on the line of the
	// call or the line above.
	allowPgx = "//gormguard:allow-pgx"
)

// instanceOwners lists the packages, relative to the module, that own the
// shared GORM instance, with the reason: they may gorm.Open, register
// callbacks and plugins, reach its *sql.DB and set its configuration.
var instanceOwners = map[string]string{
	"internal/store": "store.G's instance over the forwarding driver, with its timestamp hooks",
}

// migratedPackages lists the packages, relative to the module, whose
// statements are built with store.G: they must not go back to pgx Exec,
// Query or QueryRow. A package is added here in the batch that migrates it.
var migratedPackages = map[string]string{}

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
	r := rules{module: module, owners: instanceOwners, migrated: migratedPackages}
	known := map[string]bool{}
	var targets []listed
	for _, p := range goList(t, root, "./...") {
		rel := strings.TrimPrefix(p.ImportPath, module+"/")
		known[rel] = true
		if _, migrated := r.migrated[rel]; migrated || slices.Contains(p.Deps, gormPkg) {
			targets = append(targets, p)
		}
	}
	for _, list := range []map[string]string{instanceOwners, migratedPackages} {
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
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution|parser.ParseComments)
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
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)
`

const fixtureUses = `
var (
	_ context.Context
	_ pgx.Tx
	_ *pgxpool.Pool
	_ *gorm.DB
	_ clause.Expr
	_ store.Querier
)
`

func TestRules(t *testing.T) {
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, "-export", "-deps",
		gormPkg, clausePkg, pgxPrefix, pgxPrefix+"/pgxpool", fixtureModule+"/internal/store"))
	r := rules{
		module:   fixtureModule,
		owners:   map[string]string{"internal/store": "test"},
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
			name: "instance/outside store: the sql.DB, callbacks, plugins and configuration",
			pkg:  "internal/billing",
			src: `
func f(db *gorm.DB) {
	_, _ = db.DB()
	_ = db.Callback()
	_ = db.Use(nil)
	cb := db.Callback
	_ = cb
	db.Config = &gorm.Config{}
	*db.Config = gorm.Config{}
	db.ConnPool = nil
	db.DryRun = true
	db.ClauseBuilders["SET"] = nil
	db.Statement.ConnPool = nil
	cfg := &gorm.Config{}
	cfg.Logger = nil
}
`,
			want: []string{ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance,
				ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance},
		},
		{
			name: "instance/inside store, and statement state elsewhere",
			pkg:  "internal/store",
			src: `
func f(db *gorm.DB) {
	_ = db.Callback()
	db.ClauseBuilders["SET"] = nil
	db.Error = nil
	db.Statement.Table = "t"
}
`,
		},
		{
			name: "instance/statement state outside store passes",
			pkg:  "internal/billing",
			src: `
func f(db *gorm.DB) {
	db.Error = nil
	db.Statement.Table = "t"
	db.Statement.Vars = nil
}
`,
		},
		{
			name: "slice/every parameter position and type",
			pkg:  "internal/billing",
			src: `
type ids []int64

func f(db *gorm.DB, a []int64, b []string, c []int32, d []int, e ids, arr [2]int64, anys []any) {
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
	db.Where("id = ANY(?)", arr)
	db.Where("x IN ?", anys)
	db.Where("x IN ?", []struct{ A int }{{1}})
	db.Select("id = ANY(?) AS hit", a).Find(&rows)
	db.Model(&rows).Update("ids", a)
	db.Model(&rows).UpdateColumn("ids", a)
	db.Assign(a).Attrs(a)
	db.FirstOrInit(&rows, a)
	db.FirstOrCreate(&rows, a)
	db.Where(clause.Expr{SQL: "id = ANY(?)", Vars: []any{a}})
	db.Where(clause.Expr{"id = ANY(?)", []any{1, a}, false})
	db.Where(clause.NamedExpr{SQL: "id = ANY(@ids)", Vars: []any{a}})
}
`,
			want: []string{ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice},
		},
		{
			name: "slice/Valuers, bytes, scalars, spreads, destinations and column lists pass",
			pkg:  "internal/billing",
			imports: `import (
	"encoding/json"
	"time"
)`,
			src: `
type builder struct{}

func (builder) Where(string, ...any) {}

func f(db *gorm.DB, a []int64, raw []byte, doc json.RawMessage, at []time.Time, b builder, args []any) {
	var rows []struct{ ID int64 }
	db.Where("id = ANY(?)", store.Int64s(a)).Find(&rows)
	db.Where("id = ANY(?)", store.Int64s(nil))
	db.Where("name = ANY(?)", store.Strings{"x"})
	db.Where("at = ANY(?)", store.Times(at))
	db.Where("blob = ?", raw)
	db.Where("doc = ?", doc)
	db.Where("id = ?", a[0])
	db.Where(map[string]any{"id": 1})
	db.Where("x = ? AND y = ?", args...)
	db.Select([]string{"id", "name"}).Find(&rows)
	db.Select("id", "name").Find(&rows)
	db.Pluck("id", &a)
	db.Delete(rows)
	db.Model(&rows).Update("name", "x")
	db.Where(clause.Expr{SQL: "id = ANY(?)", Vars: []any{store.Int64s(a)}})
	db.Where(clause.Expr{SQL: "x = ?", Vars: args})
	b.Where("x", []int64{1})
}
`,
		},
		{
			name: "pgx/allow comments with a reason, on the line or the line above",
			pkg:  "internal/usage",
			src: `
func f(ctx context.Context, q store.Querier) {
	_, _ = q.Exec(ctx, "x") //gormguard:allow-pgx hot path, 1.7x the allocations on GORM
	//gormguard:allow-pgx hot path, see BASELINE.md
	_ = q.QueryRow(ctx, "x")
}
`,
		},
		{
			name: "pgx/allow comments without a reason, too far or misspelt",
			pkg:  "internal/usage",
			src: `
func f(ctx context.Context, q store.Querier) {
	_, _ = q.Exec(ctx, "x") //gormguard:allow-pgx
	//gormguard:allow-pgx far away

	_ = q.QueryRow(ctx, "x")
	_ = q.QueryRow(ctx, "x") //gormguard:allow-pgxx typo
}
`,
			want: []string{rulePgx, rulePgx, rulePgx},
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
			f, err := parser.ParseFile(fset, c.name+".go", src, parser.SkipObjectResolution|parser.ParseComments)
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

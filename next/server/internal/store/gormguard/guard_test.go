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
	// pgxModule is pgx's module path, without its major version: every
	// version's packages start with it. It is spelt in two pieces, and the
	// fixtures write it {{pgx}}, so that a search of the module's sources
	// for pgx imports finds pgerror.go alone.
	pgxModule = "github.com/jackc/" + "pgx"
)

// instanceOwners lists the packages, relative to the module, that own the
// shared GORM instance, with the reason: they may gorm.Open, register
// callbacks and plugins, reach its *sql.DB, set its configuration and
// start transactions with GORM's own Transaction and Begin.
var instanceOwners = map[string]string{
	"internal/store":          "the pool and its GORM instance (Open, with its timestamp hooks), Tx, TxOptions, WithConn and the migrator",
	"internal/testutil":       "test infrastructure: Begin for a test that ends its transaction itself, Traced's logging connection pool",
	"internal/store/gormtest": "SQL snapshots: AssertRun's statement recorder, a callback on the DryRun instance (no database behind it)",
}

// sqlOwners lists the packages, relative to the module, besides the test
// files, that may use store.DB.SQL, the pool as a database/sql handle, and
// store.SQLTx, a transaction as one.
var sqlOwners = map[string]string{
	"internal/store":    "the pool itself: Close, Ping, MaxConns, WithConn's connection",
	"internal/task":     "River's driver (riverdatabasesql) takes the *sql.DB, its InsertTx the *sql.Tx",
	"internal/testutil": "test infrastructure: seeding, Connect's single connection, ExecWithoutTrigger",
}

// pgxFiles lists the files, relative to the module, that may import pgx:
// the PostgreSQL errors GORM's driver returns are pgconn's, and their
// SQLSTATE and constraint name can only be read from that type.
var pgxFiles = map[string]string{
	"internal/store/pgerror.go": "reads *pgconn.PgError",
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
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	_, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	return info, err
}

// --- the repository ------------------------------------------------------

// TestRepositoryFollowsGORMRules checks every non-test file of the packages
// that depend on GORM or Casbin, and the imports of every Go file of the
// module, test files included ([pgx-import], [testutil-import], and
// gorm-adapter's of [casbin-api]).
func TestRepositoryFollowsGORMRules(t *testing.T) {
	root, module := moduleRoot(t)
	r := rules{module: module, owners: instanceOwners, sqlOwners: sqlOwners, pgxFiles: pgxFiles,
		casbinOwners: casbinOwners}
	known := map[string]bool{} // packages and their files, relative to the module
	var targets []listed
	for _, p := range goList(t, root, "./...") {
		rel := strings.TrimPrefix(p.ImportPath, module+"/")
		known[rel] = true
		for _, name := range p.GoFiles {
			known[rel+"/"+name] = true
		}
		if slices.ContainsFunc(p.Deps, func(d string) bool { return d == gormPkg || casbinWritePkg.MatchString(d) }) {
			targets = append(targets, p)
		}
	}
	for _, list := range []map[string]string{instanceOwners, sqlOwners, pgxFiles, casbinOwners} {
		for rel := range list {
			if !known[rel] {
				t.Errorf("%s is listed but is no package or Go file of %s; drop its entry", rel, module)
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
	scanImports(t, root, r)
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

// scanImports checks the imports of every Go file under root, whatever its
// package, build constraints or test status ([pgx-import]).
func scanImports(t *testing.T, root string, r rules) {
	t.Helper()
	fset := token.NewFileSet()
	seen := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		seen++
		for _, finding := range r.imports(fset, filepath.ToSlash(rel), f) {
			t.Error(finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no Go file under " + root)
	}
}

// --- the rules, on fixtures ----------------------------------------------

const fixtureModule = "github.com/Sub2API-Devs/sup2api/next/server"

const fixtureImports = `
import (
	"context"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)
`

const fixtureUses = `
var (
	_ context.Context
	_ *gorm.DB
	_ clause.Expr
	_ store.DB
)
`

func TestRules(t *testing.T) {
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, "-export", "-deps",
		gormPkg, clausePkg, pgxModule+"/v5", pgxModule+"/v5/pgconn", fixtureModule+"/internal/store", fixtureModule+"/"+testutilPkg))
	r := rules{
		module:    fixtureModule,
		owners:    map[string]string{"internal/store": "test"},
		sqlOwners: map[string]string{"internal/store": "test", "internal/task": "test"},
		pgxFiles:  map[string]string{"internal/store/pgerror.go": "test"},
	}
	cases := []struct {
		name    string
		pkg     string // relative to the module
		file    string // the file name, when it matters
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
			name:    "tx/queries, database/sql transactions and look-alikes pass",
			pkg:     "internal/billing",
			imports: `import "database/sql"`,
			src: `
type fake struct{}

func (fake) Begin()       {}
func (fake) Transaction() {}

func f(ctx context.Context, db *gorm.DB, pool *sql.DB, x fake) {
	var rows []struct{ ID int64 }
	db.Where("id = ?", 1).Find(&rows)
	_, _ = pool.BeginTx(ctx, nil)
	x.Begin()
	x.Transaction()
}
`,
		},
		{
			name: "tx/inside store",
			pkg:  "internal/store",
			src: `
func f(db *gorm.DB) {
	_ = db.Transaction(func(*gorm.DB) error { return nil })
	db.Begin()
}
`,
		},
		{
			name: "raw-unrun/a Raw chain read without running it",
			pkg:  "internal/billing",
			src: `
func f(db *gorm.DB) {
	_ = db.Raw("UPDATE t SET a = 1").Error
	_ = db.Raw("DELETE FROM t WHERE id = ?", 1).Where("x").RowsAffected
	var n int
	_ = db.Raw("SELECT 1").Scan(&n).Error
	_ = db.Exec("UPDATE t SET a = 1").Error
	_ = db.Where("id = ?", 1).Error
}
`,
			want: []string{ruleUnrun, ruleUnrun},
		},
		{
			name: "raw-unrun/through a variable, parentheses and a function of the package",
			pkg:  "internal/billing",
			src: `
func update(db *gorm.DB) *gorm.DB { return db.Raw("UPDATE t SET a = 1") }

func wrapped(db *gorm.DB, id int64) *gorm.DB {
	q := db.Raw("DELETE FROM t WHERE id = ?", id)
	return (q).Where("x")
}

func f(db *gorm.DB) {
	q := db.Raw("UPDATE t SET a = 1")
	_ = q.Error
	_ = q.RowsAffected
	var r *gorm.DB
	r = (db.Raw("SELECT 1"))
	_ = r.Error
	_ = (db.Raw("UPDATE t SET a = 1")).Error
	_ = (db).Raw("UPDATE t SET a = 1").Error
	_ = update(db).Error
	_ = update(db).Where("x").RowsAffected
	_ = wrapped(db, 1).Error
	s := update(db)
	_ = s.Error
}
`,
			want: []string{ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun, ruleUnrun},
		},
		{
			name: "raw-unrun/statements that ran, may have run, or are no gorm",
			pkg:  "internal/billing",
			src: `
type fake struct {
	Error        error
	RowsAffected int64
}

func (x *fake) Raw(string, ...any) *fake { return x }

func update(db *gorm.DB) *gorm.DB { return db.Raw("UPDATE t SET a = 1") }

func scanned(db *gorm.DB) *gorm.DB {
	var n int
	return db.Raw("SELECT 1").Scan(&n)
}

func either(db *gorm.DB, write bool) *gorm.DB {
	if write {
		return db.Exec("UPDATE t SET a = 1")
	}
	return db.Raw("SELECT 1")
}

func run(*gorm.DB) {}

func f(db *gorm.DB, x *fake) {
	var n int
	q := db.Raw("SELECT 1")
	q.Scan(&n)
	_ = q.Error
	r := db.Raw("SELECT 1")
	run(r)
	_ = r.Error
	s := db.Raw("SELECT 1")
	s = s.Scan(&n)
	_ = s.Error
	t := db.Raw("SELECT 1")
	rows, _ := t.Rows()
	_ = rows
	_ = t.Error
	e := db.Exec("UPDATE t SET a = 1")
	_ = e.Error
	_ = update(db).Scan(&n).Error
	_ = scanned(db).Error
	_ = either(db, true).Error
	_ = x.Raw("UPDATE t SET a = 1").Error
	_ = x.Raw("UPDATE t SET a = 1").RowsAffected
	_ = db.Error
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
			name:    "instance/session overrides and Debug outside store",
			pkg:     "internal/billing",
			imports: `import "gorm.io/gorm/logger"`,
			src: `
func f(db *gorm.DB) {
	db.Session(&gorm.Session{PrepareStmt: true})
	db.Session(&gorm.Session{NewDB: true, AllowGlobalUpdate: true, Logger: logger.Default})
	db.Session(&gorm.Session{SkipDefaultTransaction: false})
	_ = gorm.Session{DryRun: true}
	db.Debug()
	debug := db.Debug
	_ = debug
	s := &gorm.Session{}
	s.PrepareStmt = true
	db.Session(s)
}
`,
			want: []string{ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance, ruleInstance},
		},
		{
			name: "instance/plain sessions outside store, overrides inside it",
			pkg:  "internal/billing",
			src: `
func f(ctx context.Context, db *gorm.DB) {
	db.Session(&gorm.Session{NewDB: true, Context: ctx})
	db.Session(&gorm.Session{})
}
`,
		},
		{
			name: "instance/session overrides inside store",
			pkg:  "internal/store",
			src: `
func f(db *gorm.DB) {
	db.Session(&gorm.Session{DryRun: true, NewDB: true})
	db.Debug()
}
`,
		},
		{
			name: "tx-lock/functions that lock on a handle they do not check",
			pkg:  "internal/billing",
			src: `
const lockSQL = "SELECT id FROM t WHERE id = ? FOR NO KEY UPDATE"

func locked(db *gorm.DB, id int64) *gorm.DB { return db.Table("t").Where("id = ?", id).Clauses(store.ForUpdate()) }

func read(ctx context.Context, tx *gorm.DB, id int64) error {
	var n int64
	return locked(store.G(ctx, tx), id).Select("id").Take(&n).Error
}

func share(db *gorm.DB) error {
	return db.Table("t").Clauses(store.Lock(store.LockingStrengthKeyShare).Of("t")).Take(&struct{}{}).Error
}

func raw(db *gorm.DB) error {
	var n int64
	return db.Raw(lockSQL, 1).Row().Scan(&n)
}

func advisory(db *gorm.DB) error {
	return db.Exec("SELECT pg_advisory_xact_lock(?)", 1).Error
}

func literal(db *gorm.DB) error {
	return db.Table("t").Clauses(clause.Locking{Strength: "SHARE"}).Take(&struct{}{}).Error
}

func (r repo) method(tx *gorm.DB) error {
	return tx.Raw("SELECT 1 FROM t FOR key share").Row().Err()
}

type repo struct{}
`,
			want: []string{ruleTxLock, ruleTxLock, ruleTxLock, ruleTxLock, ruleTxLock, ruleTxLock},
		},
		{
			name: "tx-lock/checked handles, builders, conditional checks and functions without a handle pass",
			pkg:  "internal/billing",
			src: `
func locked(db *gorm.DB, id int64) *gorm.DB { return db.Table("t").Where("id = ?", id).Clauses(store.ForUpdate()) }

func read(ctx context.Context, tx *gorm.DB, id int64) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	var n int64
	return locked(store.G(ctx, tx), id).Select("id").Take(&n).Error
}

func maybe(db *gorm.DB, lock bool) error {
	q := db.Table("t")
	if lock {
		if err := store.RequireTx(db); err != nil {
			return err
		}
		q = q.Clauses(store.ForUpdate())
	}
	return q.Take(&struct{}{}).Error
}

func run(ctx context.Context, db *store.DB) error {
	return db.Tx(ctx, func(tx *gorm.DB) error {
		return tx.Raw("SELECT 1 FROM t FOR UPDATE").Row().Err()
	})
}

func plain(db *gorm.DB) error {
	return db.Raw("SELECT 'for updates'").Row().Err()
}
`,
		},
		{
			name: "tx-lock/test files",
			pkg:  "internal/billing",
			file: "billing_test.go",
			src:  `func f(db *gorm.DB) error { return db.Raw("SELECT 1 FOR UPDATE").Row().Err() }`,
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
	var conds store.Conds
	conds.And("id = ANY(?)", a)
}
`,
			want: []string{ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice,
				ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice},
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
	var conds store.Conds
	conds.And("id = ANY(?)", store.Int64s(a))
	conds.And("x = ? AND y = ?", args...)
}
`,
		},
		{
			name:    "slice/byte slices at a (?",
			pkg:     "internal/billing",
			imports: `import "encoding/json"`,
			src: `
const insert = "INSERT INTO t (doc) VALUES (?)"

func f(db *gorm.DB, raw []byte, doc json.RawMessage, digest [4]byte) {
	db.Exec("INSERT INTO t (a, b) VALUES (?, ?)", raw, doc)
	db.Exec(insert, doc)
	db.Where("digest IN (?)", digest)
	db.Raw("SELECT f(?)", raw)
	_ = gorm.Expr("coalesce(?, '')", doc)
	db.Where(clause.Expr{SQL: "x = (?)", Vars: []any{raw}})
	db.Where(clause.Expr{SQL: "x = ?", Vars: []any{raw}, WithoutParentheses: true})
	var conds store.Conds
	conds.And("x = (?)", raw)
	db.Select("(?) AS d", raw)
	var rows []struct{ ID int64 }
	db.Find(&rows, "x = (?)", raw)
}
`,
			want: []string{ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice, ruleSlice},
		},
		{
			name: "slice/byte slices elsewhere, Valuers, wrapped and unknown SQL pass",
			pkg:  "internal/billing",
			imports: `import (
	"database/sql/driver"
	"encoding/json"
)`,
			src: `
// bytea stands in for store.Bytes: a byte slice that is a driver.Valuer.
type bytea []byte

func (b bytea) Value() (driver.Value, error) { return []byte(b), nil }

func f(db *gorm.DB, raw []byte, doc json.RawMessage, query string, args []any) {
	db.Exec("INSERT INTO t (a, b) VALUES ( ?, ?)", raw, doc)
	db.Exec("INSERT INTO t (a) VALUES (?)", bytea(raw))
	db.Exec("INSERT INTO t (a) VALUES (?)", gorm.Expr("?", raw))
	db.Where("blob = ?", raw)
	db.Exec(query, raw)
	db.Exec("INSERT INTO t (a, b) VALUES (?, ?)", args...)
	db.Exec("UPDATE t SET a = ? WHERE b = ANY(?)", raw, store.Int64s(nil))
	db.Where(clause.Expr{SQL: "x = ?", Vars: []any{raw}})
	db.Model(&struct{}{}).Update("doc", doc)
}
`,
		},
		{
			name: "pgx-import/any pgx package, test files included",
			pkg:  "internal/usage",
			file: "usage_test.go",
			imports: `import (
	"{{pgx}}/v5"
	"{{pgx}}/v5/pgconn"
)`,
			src: `
var (
	_ pgx.Tx
	_ *pgconn.PgError
)
`,
			want: []string{rulePgx, rulePgx},
		},
		{
			name:    "pgx-import/the other files of package store",
			pkg:     "internal/store",
			file:    "db.go",
			imports: `import "{{pgx}}/v5/pgconn"`,
			src:     `var _ *pgconn.PgError`,
			want:    []string{rulePgx},
		},
		{
			name:    "pgx-import/the error file",
			pkg:     "internal/store",
			file:    "pgerror.go",
			imports: `import "{{pgx}}/v5/pgconn"`,
			src:     `var _ *pgconn.PgError`,
		},
		{
			name: "sql-handle/selectors, values, literal keys and promotion",
			pkg:  "internal/billing",
			src: `
type wrap struct{ *store.DB }

func f(ctx context.Context, db *store.DB, w wrap) {
	_ = db.SQL.PingContext(ctx)
	pool := db.SQL
	_ = pool
	_ = store.DB{Gorm: db.Gorm, SQL: nil}
	_ = w.SQL
}
`,
			want: []string{ruleSQL, ruleSQL, ruleSQL, ruleSQL},
		},
		{
			name: "sql-handle/the owners",
			pkg:  "internal/task",
			src:  `func f(ctx context.Context, db *store.DB) { _ = db.SQL.PingContext(ctx) }`,
		},
		{
			name: "sql-handle/test files",
			pkg:  "internal/billing",
			file: "billing_test.go",
			src:  `func f(ctx context.Context, db *store.DB) { _ = db.SQL.PingContext(ctx) }`,
		},
		{
			name: "sql-handle/look-alikes and the rest of store.DB pass",
			pkg:  "internal/billing",
			src: `
type q struct{ SQL string }

func f(ctx context.Context, db *store.DB, g *gorm.DB) {
	_ = q{SQL: "x"}.SQL
	_ = g.Statement.SQL.String()
	_ = db.Gorm
	_ = db.Ping(ctx)
	_ = db.MaxConns()
}
`,
		},
		{
			name: "tx/ending a transaction, savepoints and connections",
			pkg:  "internal/billing",
			src: `
func f(db *gorm.DB) {
	db.Commit()
	db.Rollback()
	db.SavePoint("s")
	db.RollbackTo("s")
	_ = db.Connection(func(*gorm.DB) error { return nil })
	commit := db.Commit
	_ = commit
}
`,
			want: []string{ruleTx, ruleTx, ruleTx, ruleTx, ruleTx, ruleTx},
		},
		{
			name:    "tx/database/sql transactions and look-alikes of the new methods pass",
			pkg:     "internal/billing",
			imports: `import "database/sql"`,
			src: `
type fake struct{}

func (fake) Commit() error     { return nil }
func (fake) Connection()       {}
func (fake) SavePoint(string)  {}

func f(tx *sql.Tx, x fake) {
	_ = tx.Commit()
	_ = tx.Rollback()
	_ = x.Commit()
	x.Connection()
	x.SavePoint("s")
}
`,
		},
		{
			name: "tx/ending one inside store",
			pkg:  "internal/store",
			src: `
func f(db *gorm.DB) {
	db.Commit()
	db.SavePoint("s")
	_ = db.Connection(func(*gorm.DB) error { return nil })
}
`,
		},
		{
			name:    "instance/reading a ConnPool outside store",
			pkg:     "internal/billing",
			imports: `import "database/sql"`,
			src: `
func f(ctx context.Context, db *gorm.DB, s *store.DB) {
	_, _ = db.Statement.ConnPool.ExecContext(ctx, "UPDATE t SET a = 1")
	_, _ = s.Gorm.Statement.ConnPool.(*sql.DB)
	pool := db.ConnPool
	_ = pool
	_ = db.Config.ConnPool
}
`,
			want: []string{ruleInstance, ruleInstance, ruleInstance, ruleInstance},
		},
		{
			name: "instance/reading a ConnPool inside store",
			pkg:  "internal/store",
			src: `
func f(ctx context.Context, db *gorm.DB) {
	_, _ = db.Statement.ConnPool.ExecContext(ctx, "UPDATE t SET a = 1")
	db.Statement.ConnPool = db.ConnPool
}
`,
		},
		{
			name: "sql-handle/store.SQLTx outside the owners, called or as a value",
			pkg:  "internal/billing",
			src: `
func f(ctx context.Context, tx *gorm.DB) {
	_, _ = store.SQLTx(tx).ExecContext(ctx, "UPDATE t SET a = 1")
	sqlTx := store.SQLTx
	_ = sqlTx
}
`,
			want: []string{ruleSQL, ruleSQL},
		},
		{
			name: "sql-handle/store.SQLTx in River's package",
			pkg:  "internal/task",
			src:  `func f(tx *gorm.DB) { _ = store.SQLTx(tx) }`,
		},
		{
			name: "sql-handle/store.SQLTx in a test file",
			pkg:  "internal/billing",
			file: "billing_test.go",
			src:  `func f(tx *gorm.DB) { _ = store.SQLTx(tx) }`,
		},
		{
			name:    "testutil-import/a production file",
			pkg:     "internal/billing",
			imports: `import "github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"`,
			src:     `var _ = testutil.DB`,
			want:    []string{ruleTestutil},
		},
		{
			name:    "testutil-import/a test file",
			pkg:     "internal/billing",
			file:    "billing_test.go",
			imports: `import "github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"`,
			src:     `var _ = testutil.DB`,
		},
		{
			name:    "testutil-import/a package of testutil",
			pkg:     "internal/testutil/fixtures",
			imports: `import "github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"`,
			src:     `var _ = testutil.DB`,
		},
		{
			name: "tx-pool/the pool inside transaction and connection callbacks",
			pkg:  "internal/billing",
			src: `
type svc struct{ db *store.DB }

type deps struct{ DB *store.DB }

func f(ctx context.Context, s *svc, d deps, db *store.DB) error {
	_ = db.Tx(ctx, func(tx *gorm.DB) error {
		if err := store.G(ctx, s.db.Gorm).Exec("UPDATE t SET a = 1").Error; err != nil {
			return err
		}
		_ = d.DB.MaxConns()
		go func() { _ = (db).Ping(ctx) }()
		return store.G(ctx, tx).Exec("UPDATE t SET b = 1").Error
	})
	_ = db.TxOptions(ctx, store.ReadOnly(), func(*gorm.DB) error { return s.db.Ping(ctx) })
	return s.db.WithConn(ctx, func(conn *gorm.DB) error {
		pool := s.db
		_ = pool
		return nil
	})
}
`,
			want: []string{ruleTxPool, ruleTxPool, ruleTxPool, ruleTxPool, ruleTxPool, ruleTxPool},
		},
		{
			name: "tx-pool/gorm's own transactions in store",
			pkg:  "internal/store",
			src: `
func f(ctx context.Context, db *store.DB) error {
	return db.WithConn(ctx, func(conn *gorm.DB) error {
		return conn.Transaction(func(tx *gorm.DB) error { return db.Ping(ctx) })
	})
}
`,
			// Reported in both callbacks: Transaction's runs inside WithConn's.
			want: []string{ruleTxPool, ruleTxPool},
		},
		{
			name: "tx-pool/the callback's handle, the pool outside, unchecked callbacks and look-alikes pass",
			pkg:  "internal/billing",
			src: `
type svc struct{ db *store.DB }

func (s *svc) work(tx *gorm.DB) error { return store.G(context.Background(), s.db.Gorm).Error }

type fake struct{ db *store.DB }

func (fake) Tx(ctx context.Context, fn func(*gorm.DB) error) error { return nil }

func run(fn func(*gorm.DB) error) error { return nil }

func f(ctx context.Context, s *svc, db *store.DB, x fake) error {
	if err := db.Ping(ctx); err != nil {
		return err
	}
	_ = db.Tx(ctx, func(tx *gorm.DB) error {
		type pools []*store.DB
		_ = pools(nil)
		return store.G(ctx, tx).Exec("UPDATE t SET a = 1").Error
	})
	_ = db.Tx(ctx, s.work)
	_ = x.Tx(ctx, func(*gorm.DB) error { return x.db.Ping(ctx) })
	return run(func(*gorm.DB) error { return db.Ping(ctx) })
}
`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\n" + fixtureImports + strings.ReplaceAll(c.imports, "{{pgx}}", pgxModule) + "\n" + c.src + fixtureUses
			name := c.file
			if name == "" {
				name = c.name + ".go"
			}
			f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution|parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			path := fixtureModule + "/" + c.pkg
			info, err := typeCheck(fset, imp, path, []*ast.File{f})
			if err != nil {
				t.Fatalf("fixture does not type-check: %v", err)
			}
			findings := append(r.imports(fset, c.pkg, f), r.check(fset, path, []*ast.File{f}, info)...)
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

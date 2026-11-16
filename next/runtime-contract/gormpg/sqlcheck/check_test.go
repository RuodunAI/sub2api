package sqlcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"slices"
	"strings"
	"testing"
)

// The fixtures' packages: app is checked; helper and model stand in for a
// module's own SQL helpers (store's DDL, Conds, NewNumbered, QuoteIdent;
// pgdb.SQL) and its generated models.
const (
	fixtureModule = "example.com/fx"
	helperSrc     = `package helper

import "gorm.io/gorm"

type SQL string

func DDL(db *gorm.DB, format string, args ...any) error { return nil }
func NewNumbered(sql string) string                    { return sql }
func Quote(s string) string                            { return s }

type Conds struct{}

func (c *Conds) And(cond string, args ...any) {}
`
	modelSrc = `package model

type T struct{}

func (T) TableName() string { return "t" }
`
	fixtureHeader = `package app

import (
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"example.com/fx/helper"
	"example.com/fx/model"
)

var (
	_ = fmt.Sprint
	_ = slices.Concat[[]string]
	_ = strings.Join
	_ *gorm.DB
	_ clause.Expr
	_ helper.SQL
	_ model.T
)
`
)

func fixturePolicy() Policy {
	return Policy{
		Module:     fixtureModule,
		Raw:        map[string]string{"app.allowedRaw": "test"},
		Dynamic:    map[string]string{"app.allowedText": "test"},
		Quoting:    map[string]string{"helper.Quote": "test"},
		Text:       map[string]bool{"helper.DDL": false, "helper.Conds.And": false, "helper.NewNumbered": true},
		ConstTypes: map[string]bool{"helper.SQL": true},
		Generated:  map[string]bool{"model": true},
	}
}

// fixtureImporter imports helper and model from source, the rest from
// export data.
type fixtureImporter struct {
	types.Importer
	pkgs map[string]*types.Package
}

func (f fixtureImporter) Import(path string) (*types.Package, error) {
	if p, ok := f.pkgs[path]; ok {
		return p, nil
	}
	return f.Importer.Import(path)
}

func newFixtureImporter(t *testing.T, fset *token.FileSet) types.Importer {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	deps, err := List(dir, "-export", "-deps", "fmt", "slices", "strings", gormPkg, clausePkg)
	if err != nil {
		t.Fatal(err)
	}
	imp := fixtureImporter{Importer: Importer(fset, deps), pkgs: map[string]*types.Package{}}
	for name, src := range map[string]string{"helper": helperSrc, "model": modelSrc} {
		f, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := (&types.Config{Importer: imp}).Check(fixtureModule+"/"+name, fset, []*ast.File{f}, nil)
		if err != nil {
			t.Fatal(err)
		}
		imp.pkgs[pkg.Path()] = pkg
	}
	return imp
}

func TestRules(t *testing.T) {
	fset := token.NewFileSet()
	imp := newFixtureImporter(t, fset)
	c := New(fixturePolicy())
	const raw, cst = RuleRawSQL, RuleSQLConst
	cases := []struct {
		name, file, src string
		want            []string
	}{
		{name: "raw-sql/Raw, Exec and a raw Text function, called or as values", src: `
var stmt = helper.NewNumbered("UPDATE t SET a = $1")

func f(db *gorm.DB) {
	var n int
	db.Raw("SELECT 1").Scan(&n)
	db.Exec("UPDATE t SET a = 1")
	exec := db.Exec
	_ = exec
}`, want: []string{raw, raw, raw, raw, cst}},
		{name: "raw-sql/set operations over builder queries pass", src: `
const keys = "(?) UNION (?) UNION (?)"

func f(db *gorm.DB, a, b *gorm.DB) {
	var n []int
	db.Raw("(?) UNION ALL (?) ORDER BY 1", a, b).Scan(&n)
	db.Raw(keys, a, b, a).Scan(&n)
	db.Raw("? UNION ? EXCEPT ALL (?)", a, b, a).Scan(&n)
	db.Raw("(?) UNION ALL (?) LIMIT 1", a, b).Scan(&n)
}`, want: []string{raw}},
		{name: "raw-sql/the allowlist and the builder", src: `
func allowedRaw(db *gorm.DB) error { return db.Exec("SELECT nextval(?)", "s").Error }

func f(db *gorm.DB) error {
	return db.Table("t").Where("id = ?", 1).Updates(map[string]any{"a": gorm.Expr("a + ?", 1)}).Error
}`},
		{name: "raw-sql/a test file", file: "x_test.go", src: `
func f(db *gorm.DB, col string) error { return db.Exec("UPDATE t SET a = 1").Where(col).Error }`},
		{name: "sql-const/text built from constants passes", src: `
const cols = "a, b"

var where = "a = ? AND " + cols + " IS NOT NULL"

func filter(col string) string { return col + " = ?" }

func order(db *gorm.DB, desc bool) *gorm.DB {
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	return db.Order("a" + dir)
}

func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

// pair returns two results, each built from constants.
func pair(desc bool) (string, []any) {
	if desc {
		return "a DESC", nil
	}
	return "a", []any{1}
}

// names lists columns from a literal.
func names() []string { return []string{"a", "b"} }

var insertColumns = []string{"a", "b"}

func selectOf(db *gorm.DB, columns ...string) *gorm.DB { return db.Select(columns) }

// entity holds SQL in unexported fields that only literals set.
type entity struct {
	name   string
	search []string
}

var entities = []*entity{{name: "e.name", search: []string{"e.alias"}}}

type pipe struct{}

func (pipe) Exec(string) error { return nil }

func f(db *gorm.DB, x int, p pipe, o helper.SQL, cond clause.Expression) {
	db.Where(cond)
	db.Table("t").Select(cols).Where(where, x).Where(filter("b"), x).Group("a").Order(clause.OrderByColumn{Column: clause.Column{Name: "a"}})
	db.Where(fmt.Sprintf("%s = ?", "a"), x)
	db.Where(strings.Join([]string{"a = ?", "b = ?"}, " AND "), x, x)
	db.Where("a IN (" + strings.TrimSuffix(strings.Repeat("?,", x), ",") + ")")
	for _, t := range []string{"t1", "t2"} {
		db.Table(t)
	}
	db.Table(helper.Quote(fmt.Sprint(x)))
	by(db, "a")
	by(db, "b")
	order(db, true)
	_ = gorm.Expr("a + ?", x)
	_ = clause.Expr{SQL: "a = ?", Vars: []any{x}}
	var c helper.Conds
	c.And("a = ?", x)
	_ = helper.DDL(db, "CREATE ROLE %s", x)
	db.Select("id", "name").Select([]string{"a", "b"}).Select(names()).Distinct("a", []string{"b"})
	db.Select("? AS v, ? AS w", x, fmt.Sprint(x))
	db.Select(insertColumns).Create(&x)
	selectOf(db, "a", "b")
	selectOf(db, []string{"c"}...)
	db.First(&x, "a = ?", x).Find(&x, x).Take(&x).Delete(&x, "a = ?", 1)
	db.InnerJoins("JOIN u ON u.id = t.u").Pluck("a", &x)
	_ = clause.Column{Name: "lower(a)", Raw: true}
	_ = clause.Table{Name: "(SELECT 1) one", Raw: true}
	_ = clause.Column{Name: fmt.Sprint(x), Raw: false}
	var ex clause.Expr
	ex.SQL = "a = ?"
	ord, args := pair(x > 0)
	db.Order(clause.OrderBy{Expression: clause.Expr{SQL: ord, Vars: args}})
	for _, e := range entities {
		for _, col := range slices.Concat([]string{e.name}, e.search) {
			db.Where(col + " = ?")
		}
	}
	var parts []string
	parts = append(parts, "a = ?", "b = ?")
	db.Where(strings.Join(parts, " AND "), x, x)
	db.Order(string(o)).Order(o + " DESC")
	var q helper.SQL = "a"
	db.Where(string(q))
	db.Table(model.T{}.TableName())
	_ = p.Exec("not gorm")
}`},
		{name: "sql-const/text built from values fails", src: `
type execer interface {
	Exec(sql string, values ...interface{}) *gorm.DB
}

type entity struct{ Name string }

func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

func f(db *gorm.DB, col string, x int, cols []string, m map[string]string, q any, e execer, en entity) {
	db.Order(col)
	db.Where(fmt.Sprintf("a = %d", x))
	db.Where("a = " + fmt.Sprint(x))
	db.Select(strings.Join(cols, ","))
	db.Order(m[col])
	by(db, col)
	_ = gorm.Expr(col)
	_ = clause.Expr{SQL: col}
	var c helper.Conds
	c.And(col)
	s := "a"
	s += col
	db.Group(s)
	_ = helper.DDL(db, col)
	_ = helper.DDL(db, "DROP ROLE "+col)
	db.Select("id", col)
	db.Select([]string{col})
	db.Select(cols)
	db.Distinct("id", col)
	db.First(&x, col)
	db.Find(&x, "a = "+col)
	db.Delete(&x, col)
	db.InnerJoins(col)
	db.Pluck(col, &cols)
	db.Where(q)
	_ = clause.Column{Name: col, Raw: true}
	_ = clause.Table{Name: "t", Alias: col, Raw: true}
	_ = clause.OrderByColumn{Column: clause.Column{Name: col, Raw: x > 0}}
	var ex clause.Expr
	ex.SQL = col
	var cc clause.Column
	cc.Raw = true
	w := db.Where
	w(col)
	(*gorm.DB).Order(db, col)
	e.Exec("UPDATE t SET a = " + col)
	db.Where(en.Name+" = ?", 1)
	_ = helper.SQL(col)
	db.Where(helper.Quote(col) + unlisted(col))
}

func unlisted(s string) string { return s }`, want: []string{
			cst, cst, cst, cst, cst, cst, cst, cst, cst, cst, // Order .. Group
			cst, cst, // DDL
			cst, cst, cst, cst, // Select, Distinct
			cst, cst, cst, // First, Find, Delete
			cst, cst, cst, // InnerJoins, Pluck, an interface
			cst, cst, cst, // raw Column, Table, OrderByColumn
			cst, cst, // ex.SQL, cc.Raw
			cst,      // w := db.Where
			cst,      // method expression
			raw, cst, // e.Exec through an interface: a statement, and its text
			cst, cst, cst, // the exported field, the conversion, an unlisted function
		}},
		{name: "sql-const/a function used as a value proves nothing of its parameters", src: `
func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

var byFn = by

func f(db *gorm.DB) { by(db, "a") }`, want: []string{cst}},
		{name: "sql-const/an allowed declaration", src: `
func allowedText(db *gorm.DB, col string) { db.Order(col) }`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.file
			if name == "" {
				name = "f.go"
			}
			f, err := parser.ParseFile(fset, name, fixtureHeader+tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			info, err := TypeCheck(fset, imp, fixtureModule+"/app", []*ast.File{f})
			if err != nil {
				t.Fatalf("fixture does not type-check: %v", err)
			}
			findings := c.Check(fset, fixtureModule+"/app", []*ast.File{f}, info)
			var got []string
			for _, finding := range findings {
				got = append(got, strings.Fields(finding[strings.Index(finding, " [")+1:])[0])
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rules %v, want %v\n%s", got, tc.want, strings.Join(findings, "\n"))
			}
		})
	}
	if unused := c.Unused(); len(unused) > 0 {
		t.Errorf("entries that matched nothing: %v", unused)
	}
}

// Unused names the entries that allowed or matched nothing: an entry for a
// declaration that no longer needs it, or that names none at all (a
// quoting helper of the same name elsewhere is not it).
func TestUnused(t *testing.T) {
	fset := token.NewFileSet()
	imp := newFixtureImporter(t, fset)
	p := fixturePolicy()
	p.Quoting = map[string]string{"helper.Quote": "test", "app.quoteLiteral": "test"}
	c := New(p)
	f, err := parser.ParseFile(fset, "f.go", fixtureHeader+`
func quoteLiteral(s string) string { return s }

func f(db *gorm.DB) { db.Table(helper.Quote("t")) }`, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := TypeCheck(fset, imp, fixtureModule+"/app", []*ast.File{f})
	if err != nil {
		t.Fatal(err)
	}
	if findings := c.Check(fset, fixtureModule+"/app", []*ast.File{f}, info); len(findings) > 0 {
		t.Fatal(findings)
	}
	want := []string{"ConstTypes helper.SQL", "Generated model", "Quoting app.quoteLiteral", "Text helper.Conds.And",
		"Text helper.DDL", "Text helper.NewNumbered", RuleRawSQL + " app.allowedRaw", RuleSQLConst + " app.allowedText"}
	if got := c.Unused(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("unused %v, want %v", got, want)
	}
}

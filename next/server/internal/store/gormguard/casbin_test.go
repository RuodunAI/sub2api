package gormguard

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// [casbin-api]: Casbin only reads the policy, which the core writes with
// GORM in the business transaction (internal/authz/casbinrule; CONTRACTS
// §99).

const (
	// casbinxPkg, relative to the module, resolves permissions with Casbin.
	casbinxPkg = "internal/authz/casbinx"
	// adapterModule is gorm-adapter's module path without its major
	// version.
	adapterModule = "github.com/casbin/gorm-adapter"
)

// casbinOwners may import gorm-adapter and call Casbin's own Enforce.
var casbinOwners = map[string]string{
	casbinxPkg: "the Casbin resolver: loads the p rows with gorm-adapter, read-only, and owns the enforcers",
}

// enforceOwners (packages or files) may call casbinx.Enforce: the matcher
// path, never a request's answer (casbinx's own tests of it). casbinx has no Enforce method to take
// over through an interface declared elsewhere (Enforce reaches its own
// resolver's unexported one, and refuses any other Resolver), so naming the
// function is the only way to the matcher.
var enforceOwners = map[string]string{
	casbinxPkg: "the resolver itself and its tests of the matcher",
}

var (
	// casbinWritePkg matches the packages whose API can write policy or own
	// a transaction: Casbin's enforcers and its persist interfaces (not its
	// in-memory model or role manager packages), and gorm-adapter.
	casbinWritePkg = regexp.MustCompile(`^github\.com/casbin/(casbin(/v\d+)?(/persist(/.*)?)?|gorm-adapter(/v\d+)?)$`)
	// casbinRootPkg matches Casbin's enforcer package.
	casbinRootPkg = regexp.MustCompile(`^github\.com/casbin/casbin(/v\d+)?$`)
	// casbinWriteAPI matches the names of the write, transaction and
	// self-reloading API: policy management (Add/Remove/Update/Delete of
	// policies, grouping policies, roles, permissions, users; their Ctx,
	// Ex and Self forms), SavePolicy, the adapter's and the transactional
	// enforcer's transactions, StartAutoLoadPolicy and the synced,
	// transactional and distributed enforcers.
	casbinWriteAPI = regexp.MustCompile(`^(Self)?(Add|Remove|Update|Delete)\w*(Polic|Role|Permission|User)|^DeleteDomains?|^SavePolicy|` +
		`^(Begin|With)?Transaction$|^Commit$|^Rollback$|^StartAutoLoadPolicy$|^New(Synced|Transactional|Distributed)`)
	// casbinEnforce matches Casbin's own matcher calls.
	casbinEnforce = regexp.MustCompile(`^(Batch)?Enforce`)
	// adapterPkg matches gorm-adapter's package.
	adapterPkg = regexp.MustCompile(`^github\.com/casbin/gorm-adapter(/v\d+)?$`)
)

// adapterAllowed is all of gorm-adapter's API that may be named, a
// whitelist: casbinx.loadPolicyRows's read-only load. The rest constructs
// adapters that AutoMigrate the table to the adapter's own struct (DDL that
// would narrow v1 and v2 back to varchar(100)), opens pools of its own
// (Open, the DSN constructors, InitDbResolver), hands the adapter's handle
// out (GetDb), loads every row (LoadPolicy: the g rows are not loaded) or
// writes. adapterReadOnly is allowed only where adapterTurnedOff holds.
var adapterAllowed = map[string]bool{
	"TurnOffAutoMigrate": true, // on the handle the adapter is then made on
	"LoadFilteredPolicy": true, // one SELECT of the p rows, inside the caller's transaction
}

// adapterReadOnly is the one gorm-adapter constructor allowed: on a handle
// TurnOffAutoMigrate ran on, it only scopes a session of it to the table
// (casbinx.loadPolicyRows). It is allowed called, with a variable as the
// handle, in a function that called TurnOffAutoMigrate on that variable
// before (adapterTurnedOff); as a value, or anywhere else, it is reported.
const (
	adapterReadOnly   = "NewAdapterByDBUseTableName"
	adapterTurnOffFun = "TurnOffAutoMigrate"
)

// listedWithin reports whether rel is, or is under, an entry of list.
func listedWithin(list map[string]string, rel string) bool {
	for k := range list {
		if within(rel, k) {
			return true
		}
	}
	return false
}

// funcPkg is the package of fn's receiver type, or of fn itself.
func funcPkg(fn *types.Func) string {
	t := types.Type(nil)
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		t = recv.Type()
	}
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := types.Unalias(t).(*types.Named); ok && n.Obj().Pkg() != nil {
		return n.Obj().Pkg().Path()
	}
	if fn.Pkg() == nil {
		return ""
	}
	return fn.Pkg().Path()
}

// casbinSelector checks the Casbin functions a selector names, so that a
// method value is caught as well as a call.
func (c *checker) casbinSelector(n *ast.SelectorExpr, fn *types.Func) {
	pkg := funcPkg(fn)
	switch {
	case casbinWritePkg.MatchString(pkg) && casbinWriteAPI.MatchString(fn.Name()):
		c.report(n, "%s Casbin %s: the core writes the policy with GORM in the business transaction (internal/authz/casbinrule); Casbin and gorm-adapter only read it", ruleCasbin, fn.Name())
	case casbinRootPkg.MatchString(pkg) && casbinEnforce.MatchString(fn.Name()) && !c.casbinOwner:
		c.report(n, "%s Casbin %s outside %s: enforcers belong to the resolver", ruleCasbin, fn.Name(), casbinxPkg)
	case adapterPkg.MatchString(pkg) && fn.Name() == adapterReadOnly && !c.adapterVetted[n]:
		c.report(n, "%s gorm-adapter %s on a handle no earlier %s of the same function ran on: the constructor AutoMigrates the table to the adapter's struct (narrowing v1 and v2) unless it did", ruleCasbin, fn.Name(), adapterTurnOffFun)
	case adapterPkg.MatchString(pkg) && fn.Name() != adapterReadOnly && !adapterAllowed[fn.Name()]:
		c.report(n, "%s gorm-adapter %s: only %s, %s (after it, on the same handle) and LoadFilteredPolicy are used; the rest AutoMigrates the table, opens pools of its own, hands out the adapter's handle, loads the g rows or writes", ruleCasbin, fn.Name(), adapterTurnOffFun, adapterReadOnly)
	case fn.Name() == "Enforce" && within(pkg, c.casbinxPkg) && !c.enforceOwner:
		c.report(n, "%s casbinx Enforce outside casbinx: it runs every policy through the matcher (8-230 µs); a request asks PermissionSet.Has", ruleCasbin)
	}
}

// casbinCall reports EnableAutoSave with anything but the constant false:
// an enforcer with auto-save writes every policy change through its adapter.
// It vets a call of adapterReadOnly for casbinSelector, which sees the
// selector after the call (ast.Inspect visits the call first).
func (c *checker) casbinCall(fn *types.Func, n *ast.CallExpr) {
	if fn.Name() == adapterReadOnly && adapterPkg.MatchString(funcPkg(fn)) && c.adapterTurnedOff(n) {
		c.adapterVetted[ast.Unparen(n.Fun).(*ast.SelectorExpr)] = true
	}
	if fn.Name() != "EnableAutoSave" || !casbinRootPkg.MatchString(funcPkg(fn)) || len(n.Args) != 1 {
		return
	}
	if tv, ok := c.info.Types[n.Args[0]]; ok && tv.Value != nil && tv.Value.Kind() == constant.Bool && !constant.BoolVal(tv.Value) {
		return
	}
	c.report(n, "%s EnableAutoSave with anything but false: an enforcer never writes through its adapter", ruleCasbin)
}

// adapterTurnedOff reports whether the handle call (adapterReadOnly) is
// made on is a variable TurnOffAutoMigrate ran on before, in the function
// (or function literal) the call is in, not in one nested in it.
func (c *checker) adapterTurnedOff(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	handle := c.identObj(call.Args[0])
	body := c.enclosingBody(call.Pos())
	if handle == nil || body == nil {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
			if !ok || n.End() > call.Pos() || len(n.Args) != 1 {
				return true
			}
			if fn := funcOf(c.info, sel); fn != nil && fn.Name() == adapterTurnOffFun && adapterPkg.MatchString(funcPkg(fn)) &&
				c.identObj(n.Args[0]) == handle {
				found = true
			}
		}
		return !found
	})
	return found
}

// identObj is the object e names when it is a plain identifier.
func (c *checker) identObj(e ast.Expr) types.Object {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	return c.info.Uses[id]
}

// enclosingBody is the body of the innermost function or function literal
// of the package holding pos.
func (c *checker) enclosingBody(pos token.Pos) *ast.BlockStmt {
	var in *ast.BlockStmt
	for _, b := range c.bodies {
		if b.Pos() <= pos && pos < b.End() && (in == nil || b.Pos() >= in.Pos()) {
			in = b
		}
	}
	return in
}

// --- the rule, on fixtures -------------------------------------------------

// TestCasbinxEnforceRule needs no Casbin: casbinx.Enforce is the module's
// own function.
func TestCasbinxEnforceRule(t *testing.T) {
	casbinFixtures(t, nil, []casbinCase{
		{name: "outside the owners, called or as a value", pkg: "internal/httpapi", want: []string{ruleCasbin, ruleCasbin}, src: `
func f(ctx context.Context, r casbinx.Resolver) {
	_, _, _ = casbinx.Enforce(ctx, r, 1, "x", 0)
	e := casbinx.Enforce
	_ = e
	_, _ = r.Grants(ctx, 1, 0)
}`},
		{name: "another file of authz", pkg: "internal/authz", file: "service.go", want: []string{ruleCasbin}, src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _, _ = casbinx.Enforce(ctx, r, 1, "x", 0) }`},
		{name: "a package of casbinx", pkg: casbinxPkg + "/x", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _, _ = casbinx.Enforce(ctx, r, 1, "x", 0) }`},
		{name: "the server CLI", pkg: "cmd/sub2api", want: []string{ruleCasbin}, src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _, _ = casbinx.Enforce(ctx, r, 1, "x", 0) }`},
		{name: "a test file", pkg: "internal/httpapi", file: "x_test.go", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _, _ = casbinx.Enforce(ctx, r, 1, "x", 0) }`},
		{name: "look-alikes", pkg: "internal/httpapi", src: `
type fake struct{}

func (fake) Enforce(context.Context, int64, string, int64) (bool, int64, error) { return false, 0, nil }

func Enforce(context.Context, casbinx.Resolver, int64, string, int64) (bool, int64, error) { return false, 0, nil }

func f(ctx context.Context, x fake, r casbinx.Resolver) {
	_, _, _ = x.Enforce(ctx, 1, "x", 0)
	_, _, _ = Enforce(ctx, r, 1, "x", 0)
}`},
	})
}

// No interface declared outside casbinx takes the matcher path over: the
// Resolver has no Enforce method, exported or not (the matcher is the
// unexported method of casbinx's own resolver, which only casbinx.Enforce
// reaches), so a Resolver does not satisfy an interface naming either.
func TestCasbinxEnforceNotTakenOver(t *testing.T) {
	for name, src := range map[string]string{
		"exported": `
type enforcer interface {
	Enforce(context.Context, int64, string, int64) (bool, int64, error)
}

var _ enforcer = casbinx.Resolver(nil)`,
		"unexported": `
type enforcer interface {
	enforce(context.Context, int64, string, int64) (bool, int64, error)
}

var _ enforcer = casbinx.Resolver(nil)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := casbinFixture(t, nil, casbinCase{pkg: "internal/httpapi", src: src})
			if err == nil || !strings.Contains(err.Error(), "does not implement") {
				t.Fatalf("a local interface took casbinx's matcher path over: %v", err)
			}
		})
	}
}

const (
	casbinPkgPath  = "github.com/casbin/casbin/v3"
	adapterPkgPath = adapterModule + "/v3"
)

// TestCasbinAPIRule runs once the module requires Casbin and gorm-adapter
// (their export data type-checks the fixtures).
func TestCasbinAPIRule(t *testing.T) {
	root, _ := moduleRoot(t)
	for _, m := range []string{casbinPkgPath, adapterPkgPath} {
		cmd := exec.Command("go", "list", "-m", m)
		cmd.Dir = root
		if err := cmd.Run(); err != nil {
			t.Skipf("the module does not require %s", m)
		}
	}
	imports := `import (
	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	gormadapter "` + adapterPkgPath + `"
)`
	uses := `
var (
	_ *casbin.Enforcer
	_ model.Model
	_ persist.Adapter
	_ *gormadapter.Adapter
)
`
	casbinFixtures(t, []string{casbinPkgPath, casbinPkgPath + "/model", casbinPkgPath + "/persist", adapterPkgPath}, []casbinCase{
		{name: "write, transaction and reloading API, in casbinx too", pkg: casbinxPkg + "/x", imports: imports, src: uses + `
func f(ctx context.Context, e *casbin.Enforcer, ie casbin.IEnforcer, a *gormadapter.Adapter, pa persist.Adapter, m model.Model) {
	_, _ = e.AddPolicy("r:x", "a", "*")
	_, _ = e.AddNamedPolicies("p", [][]string{{"r:x", "a", "*"}})
	_, _ = e.RemoveFilteredPolicy(0, "r:x")
	_, _ = ie.UpdatePolicy([]string{"a"}, []string{"b"})
	_, _ = e.AddRoleForUser("u:1", "r:x")
	_ = e.SavePolicy()
	_ = pa.AddPolicy("p", "p", []string{"r:x"})
	_ = a.Transaction(e, func(casbin.IEnforcer) error { return nil })
	_, _ = casbin.NewSyncedEnforcer()
	_, _ = casbin.NewTransactionalEnforcer()
	add := e.AddPolicies
	_ = add
	e.EnableAutoSave(true)
	on := true
	e.EnableAutoSave(on)
	e.EnableAutoSave(false)
	_ = m.AddPolicies("p", "p", [][]string{{"r:x", "a", "*"}})
	e.AddFunction("permMatch", nil)
	_ = e.LoadPolicy()
	_ = e.BuildRoleLinks()
	_, _ = e.Enforce("u:1", "a")
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin,
			ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin}},
		{name: "gorm-adapter constructors, in casbinx too", pkg: casbinxPkg + "/x", imports: imports, src: uses + `
func f(db *gorm.DB) {
	_, _ = gormadapter.NewAdapter("postgres", "dsn")
	_, _ = gormadapter.NewAdapterByDB(db)
	_, _ = gormadapter.NewFilteredAdapterByDB(db, "", "casbin_rule")
	_, _ = gormadapter.NewAdapterByDBWithCustomTable(db, nil, "casbin_rule")
	_, _ = gormadapter.NewTransactionalAdapterByDB(db)
	_, _ = gormadapter.InitDbResolver(nil, nil)
	newAdapter := gormadapter.NewAdapterByDB
	_ = newAdapter
	gormadapter.TurnOffAutoMigrate(db)
	_, _ = gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin}},
		{name: "gorm-adapter is a whitelist: the read-only load alone", pkg: casbinxPkg + "/x", imports: imports, src: uses + `
func load(tx *gorm.DB, m model.Model) error {
	h := tx.Session(&gorm.Session{})
	gormadapter.TurnOffAutoMigrate(h)
	a, err := gormadapter.NewAdapterByDBUseTableName(h, "", "casbin_rule")
	if err != nil {
		return err
	}
	return a.LoadFilteredPolicy(m, gormadapter.Filter{Ptype: []string{"p"}})
}

func probes(db, other *gorm.DB, a *gormadapter.Adapter, m model.Model) {
	_, _ = gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	gormadapter.TurnOffAutoMigrate(other)
	_, _ = gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	func() { gormadapter.TurnOffAutoMigrate(db) }()
	_, _ = gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	_, _ = gormadapter.NewAdapterByDBUseTableName(other.Session(&gorm.Session{}), "", "casbin_rule")
	newAdapter := gormadapter.NewAdapterByDBUseTableName
	_ = newAdapter
	_ = a.Open()
	a.GetDb().Exec("DELETE FROM casbin_rule")
	_ = a.LoadPolicy(m)
	_ = a.Close()
	_ = a.Copy()
}

func later(db *gorm.DB) {
	_, _ = gormadapter.NewAdapterByDBUseTableName(db, "", "casbin_rule")
	gormadapter.TurnOffAutoMigrate(db)
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin,
			ruleCasbin, ruleCasbin}},
		{name: "Casbin's Enforce outside casbinx", pkg: "internal/authz", file: "service.go", imports: imports, src: uses + `
func f(e *casbin.Enforcer) {
	_, _ = e.Enforce("u:1", "a")
	_, _, _ = e.EnforceEx("u:1", "a")
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin}}, // the import, then both calls
		{name: "gorm-adapter imported outside casbinx, test files included", pkg: "internal/authz", file: "x_test.go", imports: imports, src: uses,
			want: []string{ruleCasbin}},
	})
}

type casbinCase struct {
	name, pkg, file, imports, src string
	want                          []string
}

// casbinFixtures type-checks each case (with the export data of extra) as
// a file of its package and compares the [casbin-api] findings.
func casbinFixtures(t *testing.T, extra []string, cases []casbinCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			findings, err := casbinFixture(t, extra, c)
			if err != nil {
				t.Fatalf("fixture does not type-check: %v", err)
			}
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

// casbinFixture type-checks c and returns its findings (imports, then the
// checks).
func casbinFixture(t *testing.T, extra []string, c casbinCase) ([]string, error) {
	t.Helper()
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, append([]string{"-export", "-deps",
		gormPkg, clausePkg, fixtureModule + "/internal/store", fixtureModule + "/" + casbinxPkg}, extra...)...))
	r := rules{module: fixtureModule, casbinOwners: casbinOwners, enforceOwners: enforceOwners}
	src := "package p\n" + fixtureImports + `import "` + fixtureModule + "/" + casbinxPkg + `"` + "\n" + c.imports + "\n" + c.src + fixtureUses +
		"var _ casbinx.Resolver\n"
	name := c.file
	if name == "" {
		name = "f.go"
	}
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	path := fixtureModule + "/" + c.pkg
	info, err := typeCheck(fset, imp, path, []*ast.File{f})
	if err != nil {
		return nil, err
	}
	return append(r.imports(fset, c.pkg, f), r.check(fset, path, []*ast.File{f}, info)...), nil
}

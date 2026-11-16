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

// [casbin-api]: Casbin never writes the policy, which the core writes with
// GORM in the business transaction (internal/authz/casbinrule; CONTRACTS
// §99.6). The node's enforcer (casbinx) holds the policy in memory: it may
// edit that copy (a user's g rows), on an enforcer made without an adapter;
// nothing may persist, open a transaction, watch, dispatch or reload by
// itself.

const (
	// casbinxPkg, relative to the module, resolves permissions with Casbin.
	casbinxPkg = "internal/authz/casbinx"
	// adapterModule is gorm-adapter's module path without its major
	// version.
	adapterModule = "github.com/casbin/gorm-adapter"
)

// casbinOwners may import gorm-adapter and call Casbin's own Enforce.
var casbinOwners = map[string]string{
	casbinxPkg: "the node's Casbin enforcer: loads the policy with gorm-adapter, read-only, and owns the enforcer and its in-memory policy",
}

var (
	// casbinWritePkg matches the packages whose API can write policy or own
	// a transaction: Casbin's enforcers and its persist interfaces (not its
	// in-memory model or role manager packages), and gorm-adapter.
	casbinWritePkg = regexp.MustCompile(`^github\.com/casbin/(casbin(/v\d+)?(/persist(/.*)?)?|gorm-adapter(/v\d+)?)$`)
	// casbinRootPkg matches Casbin's enforcer package.
	casbinRootPkg = regexp.MustCompile(`^github\.com/casbin/casbin(/v\d+)?$`)
	// casbinWriteAPI matches the names of the write, transaction, watching
	// and self-reloading API: policy management (Add/Remove/Update/Delete
	// of policies, grouping policies, roles, permissions, users; their Ctx,
	// Ex and Self forms) but casbinMemoryAPI in casbinx, SavePolicy and
	// LoadPolicy, the adapter's and the transactional enforcer's
	// transactions, StartAutoLoadPolicy, adapters, watchers and dispatchers
	// set on an enforcer, and the enforcer constructors but casbinCtor's
	// one-argument call in casbinx.
	casbinWriteAPI = regexp.MustCompile(`^(Self)?(Add|Remove|Update|Delete)\w*(Polic|Role|Permission|User)|^DeleteDomains?|^(Save|Load)Policy$|` +
		`^(Begin|With)?Transaction$|^Commit$|^Rollback$|^StartAutoLoadPolicy$|^Set(Adapter|Watcher|Dispatcher)$|^New\w*Enforcer$`)
	// casbinMemoryAPI matches the edits of the enforcer's in-memory policy
	// casbinx makes (a user's g rows when the user's roles or status
	// change): on an enforcer without an adapter and with auto-save off
	// they reach nothing but memory.
	casbinMemoryAPI = regexp.MustCompile(`^(Add|Remove)(Named)?(Grouping)?Polic(y|ies)$|^RemoveFiltered(Named)?(Grouping)?Policy$`)
	// casbinCtor matches the enforcer constructors casbinx may call, with
	// the model as their one argument: an enforcer without an adapter.
	casbinCtor = regexp.MustCompile(`^New(Synced)?(Cached)?Enforcer$`)
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
// out (GetDb), loads outside the caller's snapshot transaction (LoadPolicy)
// or writes. adapterReadOnly is allowed only where adapterTurnedOff holds.
var adapterAllowed = map[string]bool{
	"TurnOffAutoMigrate": true, // on the handle the adapter is then made on
	"LoadFilteredPolicy": true, // one SELECT of the p and g rows, inside the caller's transaction
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
	case casbinRootPkg.MatchString(pkg) && c.casbinOwner && casbinMemoryAPI.MatchString(fn.Name()):
	case casbinRootPkg.MatchString(pkg) && c.casbinCtorVetted[n]:
	case casbinWritePkg.MatchString(pkg) && casbinWriteAPI.MatchString(fn.Name()):
		c.report(n, "%s Casbin %s: the core writes the policy with GORM in the business transaction (internal/authz/casbinrule); Casbin and gorm-adapter only read it", ruleCasbin, fn.Name())
	case casbinRootPkg.MatchString(pkg) && casbinEnforce.MatchString(fn.Name()) && !c.casbinOwner:
		c.report(n, "%s Casbin %s outside %s: enforcers belong to the resolver", ruleCasbin, fn.Name(), casbinxPkg)
	case adapterPkg.MatchString(pkg) && fn.Name() == adapterReadOnly && !c.adapterVetted[n]:
		c.report(n, "%s gorm-adapter %s on a handle no earlier %s of the same function ran on: the constructor AutoMigrates the table to the adapter's struct (narrowing v1 and v2) unless it did", ruleCasbin, fn.Name(), adapterTurnOffFun)
	case adapterPkg.MatchString(pkg) && fn.Name() != adapterReadOnly && !adapterAllowed[fn.Name()]:
		c.report(n, "%s gorm-adapter %s: only %s, %s (after it, on the same handle) and LoadFilteredPolicy are used; the rest AutoMigrates the table, opens pools of its own, hands out the adapter's handle, loads outside the snapshot or writes", ruleCasbin, fn.Name(), adapterTurnOffFun, adapterReadOnly)
	}
}

// casbinCall reports EnableAutoSave with anything but the constant false:
// an enforcer with auto-save writes every policy change through its adapter.
// It vets a call of adapterReadOnly, and casbinx's one-argument call of an
// enforcer constructor, for casbinSelector, which sees the selector after
// the call (ast.Inspect visits the call first).
func (c *checker) casbinCall(fn *types.Func, n *ast.CallExpr) {
	if fn.Name() == adapterReadOnly && adapterPkg.MatchString(funcPkg(fn)) && c.adapterTurnedOff(n) {
		c.adapterVetted[ast.Unparen(n.Fun).(*ast.SelectorExpr)] = true
	}
	if sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr); ok && c.casbinOwner && len(n.Args) == 1 &&
		casbinCtor.MatchString(fn.Name()) && casbinRootPkg.MatchString(funcPkg(fn)) {
		c.casbinCtorVetted[sel] = true
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
		{name: "write, transaction, watching and reloading API, in casbinx too", pkg: casbinxPkg + "/x", imports: imports, src: uses + `
func f(ctx context.Context, e *casbin.Enforcer, se *casbin.SyncedCachedEnforcer, ie casbin.IEnforcer, a *gormadapter.Adapter, pa persist.Adapter, w persist.Watcher, m model.Model) {
	_, _ = ie.UpdatePolicy([]string{"a"}, []string{"b"})
	_, _ = e.AddRoleForUser("u:1", "r:x")
	_ = e.SavePolicy()
	_ = e.LoadPolicy()
	_ = pa.AddPolicy("p", "p", []string{"r:x"})
	_ = a.Transaction(e, func(casbin.IEnforcer) error { return nil })
	_, _ = casbin.NewSyncedEnforcer()
	_, _ = casbin.NewSyncedCachedEnforcer(m, pa)
	_, _ = casbin.NewTransactionalEnforcer(m)
	newEnforcer := casbin.NewSyncedCachedEnforcer
	_ = newEnforcer
	e.SetAdapter(pa)
	_ = e.SetWatcher(w)
	se.StartAutoLoadPolicy(0)
	e.EnableAutoSave(true)
	on := true
	e.EnableAutoSave(on)
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin,
			ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin, ruleCasbin}},
		{name: "casbinx's enforcer: no adapter, its memory edited", pkg: casbinxPkg + "/x", imports: imports, src: uses + `
func f(m model.Model) {
	e, _ := casbin.NewSyncedCachedEnforcer(m)
	e.EnableAutoSave(false)
	e.AddFunction("permMatch", nil)
	_, _ = e.AddGroupingPolicies([][]string{{"u:1", "r:x"}})
	_, _ = e.RemoveFilteredGroupingPolicy(0, "u:1")
	_, _ = e.AddPolicy("r:x", "a", "*")
	_, _ = e.AddNamedPolicies("p", [][]string{{"r:x", "a", "*"}})
	_, _ = e.RemoveFilteredPolicy(0, "r:x")
	add := e.AddPolicies
	_ = add
	_ = m.AddPolicies("p", "p", [][]string{{"r:x", "a", "*"}})
	_ = e.BuildRoleLinks()
	_, _ = e.Enforce("u:1", "a")
	_, _ = e.GetImplicitPermissionsForUser("u:1")
}`},
		{name: "the enforcer's memory outside casbinx", pkg: "internal/authz", file: "service.go", imports: imports, src: uses + `
func f(m model.Model, e *casbin.SyncedCachedEnforcer) {
	_, _ = casbin.NewSyncedCachedEnforcer(m)
	_, _ = e.AddGroupingPolicies([][]string{{"u:1", "r:x"}})
}`, want: []string{ruleCasbin, ruleCasbin, ruleCasbin}}, // the import, then both calls
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
		gormPkg, clausePkg, fixtureModule + "/internal/store"}, extra...)...))
	r := rules{module: fixtureModule, casbinOwners: casbinOwners}
	src := "package p\n" + fixtureImports + c.imports + "\n" + c.src + fixtureUses
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

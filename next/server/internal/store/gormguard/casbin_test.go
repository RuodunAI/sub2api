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

// [casbin-api]: Casbin only reads the policy, which the role tables'
// triggers write in the writer's transaction (CONTRACTS §99).

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

// enforceOwners (packages or files) may call the Enforce of casbinx's
// Resolver and Snapshot: the matcher path, never a request's answer.
var enforceOwners = map[string]string{
	casbinxPkg:                 "the resolver itself and its tests of the matcher",
	"internal/authz/shadow.go": "the shadow comparison samples the matcher path against PermissionSet.Has",
	"cmd/sub2api":              "admin authz-verify compares the matcher path with the legacy loader",
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
		c.report(n, "%s Casbin %s: the policy is the role tables' projection, written by their triggers in the writer's transaction; Casbin and gorm-adapter only read it", ruleCasbin, fn.Name())
	case casbinRootPkg.MatchString(pkg) && casbinEnforce.MatchString(fn.Name()) && !c.casbinOwner:
		c.report(n, "%s Casbin %s outside %s: enforcers belong to the resolver", ruleCasbin, fn.Name(), casbinxPkg)
	case fn.Name() == "Enforce" && within(pkg, c.casbinxPkg) && !c.enforceOwner:
		c.report(n, "%s casbinx Enforce outside the shadow comparison, casbinx and authz-verify: it runs every policy through the matcher (8-230 µs); a request asks PermissionSet.Has", ruleCasbin)
	}
}

// casbinCall reports EnableAutoSave with anything but the constant false:
// an enforcer with auto-save writes every policy change through its adapter.
func (c *checker) casbinCall(fn *types.Func, n *ast.CallExpr) {
	if fn.Name() != "EnableAutoSave" || !casbinRootPkg.MatchString(funcPkg(fn)) || len(n.Args) != 1 {
		return
	}
	if tv, ok := c.info.Types[n.Args[0]]; ok && tv.Value != nil && tv.Value.Kind() == constant.Bool && !constant.BoolVal(tv.Value) {
		return
	}
	c.report(n, "%s EnableAutoSave with anything but false: an enforcer never writes through its adapter", ruleCasbin)
}

// --- the rule, on fixtures -------------------------------------------------

// TestCasbinxEnforceRule needs no Casbin: casbinx's Resolver is the
// module's own interface.
func TestCasbinxEnforceRule(t *testing.T) {
	casbinFixtures(t, nil, []casbinCase{
		{name: "outside the owners, called or as a value", pkg: "internal/httpapi", want: []string{ruleCasbin, ruleCasbin}, src: `
func f(ctx context.Context, r casbinx.Resolver) {
	_, _ = r.Enforce(ctx, 1, "x", 0)
	e := r.Enforce
	_ = e
	_, _ = r.Grants(ctx, 1, 0)
}`},
		{name: "another file of authz", pkg: "internal/authz", file: "service.go", want: []string{ruleCasbin}, src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _ = r.Enforce(ctx, 1, "x", 0) }`},
		{name: "the shadow comparison", pkg: "internal/authz", file: "shadow.go", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _ = r.Enforce(ctx, 1, "x", 0) }`},
		{name: "a package of casbinx", pkg: casbinxPkg + "/x", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _ = r.Enforce(ctx, 1, "x", 0) }`},
		{name: "authz-verify", pkg: "cmd/sub2api", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _ = r.Enforce(ctx, 1, "x", 0) }`},
		{name: "a test file", pkg: "internal/httpapi", file: "x_test.go", src: `
func f(ctx context.Context, r casbinx.Resolver) { _, _ = r.Enforce(ctx, 1, "x", 0) }`},
		{name: "look-alikes", pkg: "internal/httpapi", src: `
type fake struct{}

func (fake) Enforce(context.Context, int64, string, int64) (bool, error) { return false, nil }

func f(ctx context.Context, x fake) { _, _ = x.Enforce(ctx, 1, "x", 0) }`},
	})
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
		{name: "Casbin's Enforce outside casbinx", pkg: "internal/authz", file: "shadow.go", imports: imports, src: uses + `
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
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, append([]string{"-export", "-deps",
		gormPkg, clausePkg, fixtureModule + "/internal/store", fixtureModule + "/" + casbinxPkg}, extra...)...))
	r := rules{module: fixtureModule, casbinOwners: casbinOwners, enforceOwners: enforceOwners}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
				t.Fatalf("fixture does not type-check: %v", err)
			}
			var got []string
			findings := append(r.imports(fset, c.pkg, f), r.check(fset, path, []*ast.File{f}, info)...)
			for _, finding := range findings {
				got = append(got, strings.Fields(finding[strings.Index(finding, " [")+1:])[0])
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("rules %v, want %v\n%s", got, c.want, strings.Join(findings, "\n"))
			}
		})
	}
}

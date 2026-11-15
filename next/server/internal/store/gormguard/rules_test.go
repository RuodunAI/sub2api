package gormguard

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

// Rule tags, at the start of every finding.
const (
	ruleTx       = "[gorm-tx]"
	ruleOpen     = "[gorm-open]"
	ruleInstance = "[gorm-instance]"
	ruleSlice    = "[gorm-slice]"
	rulePgx      = "[pgx-direct]"
)

// bannedMethods are the gorm (*DB) methods that would let GORM own a
// transaction or the schema.
var bannedMethods = map[string]bool{"Transaction": true, "Begin": true, "AutoMigrate": true, "Migrator": true}

// instanceMethods are the gorm (*DB) methods that reach or change the
// shared instance behind every handle: its *sql.DB, callbacks and plugins.
var instanceMethods = map[string]bool{"DB": true, "Callback": true, "Use": true}

// conditionMethods are the gorm (*DB) methods whose arguments become SQL
// parameters, by the index of the first such argument: Find and the other
// finders take their destination first, Update and UpdateColumn the column
// name. Select is handled apart (see parameterArgs).
var conditionMethods = map[string]int{
	"Where": 0, "Or": 0, "Not": 0, "Having": 0, "Raw": 0, "Exec": 0, "Joins": 0, "Assign": 0, "Attrs": 0,
	"Find": 1, "First": 1, "Take": 1, "Last": 1, "Delete": 1, "FirstOrInit": 1, "FirstOrCreate": 1,
	"Update": 1, "UpdateColumn": 1,
}

// pgxMethods are the direct statement calls of a pgx querier.
var pgxMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true}

type rules struct {
	module   string
	owners   map[string]string
	migrated map[string]string
}

// check returns the findings of one type-checked package as
// "file:line:col: [rule] message". The files must be parsed with comments.
func (r rules) check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) []string {
	rel := strings.TrimPrefix(pkgPath, r.module+"/")
	_, owner := r.owners[rel]
	_, migrated := r.migrated[rel]
	c := &checker{fset: fset, info: info, owner: owner, migrated: migrated}
	for _, f := range files {
		c.allowed = allowComments(fset, f)
		ast.Inspect(f, c.visit)
	}
	return c.out
}

type checker struct {
	fset            *token.FileSet
	info            *types.Info
	owner, migrated bool
	allowed         map[int]string // line -> reason of an allow-pgx comment
	out             []string
}

func (c *checker) report(n ast.Node, format string, args ...any) {
	c.out = append(c.out, fmt.Sprintf("%s: %s", c.fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
}

func (c *checker) visit(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.SelectorExpr:
		c.selector(n)
	case *ast.CallExpr:
		c.call(n)
	case *ast.AssignStmt:
		c.assign(n)
	case *ast.CompositeLit:
		c.literal(n)
	}
	return true
}

// selector checks the functions a selector names: selectors, not calls, so
// that a method value (f := db.Begin) is caught as well.
func (c *checker) selector(n *ast.SelectorExpr) {
	fn := funcOf(c.info, n)
	switch {
	case fn == nil:
	case isGormMethod(fn) && bannedMethods[fn.Name()]:
		c.report(n, "%s gorm (*DB).%s: transactions start in store.DB.Tx and the schema comes from the SQL migrations", ruleTx, fn.Name())
	case isGormMethod(fn) && instanceMethods[fn.Name()] && !c.owner:
		c.report(n, "%s gorm (*DB).%s outside package store: every handle shares store.G's instance", ruleInstance, fn.Name())
	case isGormFunc(fn, "Open") && !c.owner:
		c.report(n, "%s gorm.Open outside package store: take a handle from store.G", ruleOpen)
	case c.migrated && isPgxStatement(fn):
		c.pgx(n, fn)
	}
}

func (c *checker) pgx(n *ast.SelectorExpr, fn *types.Func) {
	line := c.fset.Position(n.Pos()).Line
	reason, ok := c.allowed[line]
	if !ok {
		reason, ok = c.allowed[line-1]
	}
	switch {
	case ok && reason != "":
	case ok:
		c.report(n, "%s %s without a reason: say why %s stays on pgx", rulePgx, allowPgx, fn.Name())
	default:
		c.report(n, "%s pgx %s in a migrated package: build the statement with store.G (Batch and CopyFrom stay on pgx; a measured hot path takes %s <reason>)",
			rulePgx, fn.Name(), allowPgx)
	}
}

// allowComments maps the lines of the allow-pgx comments of f to their
// reasons.
func allowComments(fset *token.FileSet, f *ast.File) map[int]string {
	out := map[int]string{}
	for _, g := range f.Comments {
		for _, cm := range g.List {
			rest, ok := strings.CutPrefix(cm.Text, allowPgx)
			if ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
				out[fset.Position(cm.Slash).Line] = strings.TrimSpace(rest)
			}
		}
	}
	return out
}

func (c *checker) call(n *ast.CallExpr) {
	sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	fn := funcOf(c.info, sel)
	if fn == nil {
		return
	}
	for _, arg := range c.parameterArgs(fn, n) {
		c.checkParam(arg, "gorm "+fn.Name())
	}
}

// parameterArgs returns the arguments of a gorm call that become SQL
// parameters. A spread (args...) passes its elements, typed any, so it is
// left out.
func (c *checker) parameterArgs(fn *types.Func, n *ast.CallExpr) []ast.Expr {
	args := n.Args
	if n.Ellipsis.IsValid() {
		args = args[:len(args)-1]
	}
	switch {
	case isGormFunc(fn, "Expr"):
		return args
	case !isGormMethod(fn):
		return nil
	case fn.Name() == "Select":
		// Select("a = ANY(?)", ids) is a fragment with parameters;
		// Select([]string{"id"}) and Select("id", "name") list columns.
		if len(n.Args) == 0 || !isString(c.info.TypeOf(n.Args[0])) {
			return nil
		}
		return tail(args, 1)
	}
	if first, ok := conditionMethods[fn.Name()]; ok {
		return tail(args, first)
	}
	return nil
}

// literal checks the Vars of a clause.Expr or clause.NamedExpr literal.
func (c *checker) literal(n *ast.CompositeLit) {
	t := c.info.TypeOf(n)
	if !isNamed(t, clausePkg, "Expr") && !isNamed(t, clausePkg, "NamedExpr") {
		return
	}
	vars, ok := ast.Unparen(fieldValue(n, "Vars", 1)).(*ast.CompositeLit)
	if !ok {
		return
	}
	for _, e := range vars.Elts {
		c.checkParam(e, t.String()+".Vars")
	}
}

func (c *checker) checkParam(arg ast.Expr, where string) {
	if t := c.info.TypeOf(arg); expandedSlice(t) {
		c.report(arg, "%s %s argument to %s: GORM expands it into a list; pass store.Int64s, store.Strings or store.Times", ruleSlice, t, where)
	}
}

// assign checks assignments to the configuration of the shared instance.
func (c *checker) assign(n *ast.AssignStmt) {
	if c.owner {
		return
	}
	for _, lhs := range n.Lhs {
		if v := assignedField(c.info, lhs); v != nil && instanceField(v) {
			c.report(lhs, "%s assignment to gorm %s outside package store: every handle shares store.G's instance", ruleInstance, v.Name())
		}
	}
}

// assignedField is the struct field an assignment target writes, through
// indexing (a map field) and dereferencing.
func assignedField(info *types.Info, e ast.Expr) *types.Var {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.SelectorExpr:
			if s := info.Selections[x]; s != nil && s.Kind() == types.FieldVal {
				v, _ := s.Obj().(*types.Var)
				return v
			}
			return nil
		default:
			return nil
		}
	}
}

// instanceField reports a field of gorm.Config (promoted into gorm.DB or
// not), gorm.DB's Config itself, or a ConnPool.
func instanceField(v *types.Var) bool {
	if v.Pkg() == nil || v.Pkg().Path() != gormPkg {
		return false
	}
	if v.Name() == "Config" || v.Name() == "ConnPool" {
		return true
	}
	tn, _ := v.Pkg().Scope().Lookup("Config").(*types.TypeName)
	if tn == nil {
		return false
	}
	st, _ := tn.Type().Underlying().(*types.Struct)
	for i := range st.NumFields() {
		if st.Field(i) == v {
			return true
		}
	}
	return false
}

// fieldValue is the value of field name in a struct literal, keyed or at
// position pos.
func fieldValue(n *ast.CompositeLit, name string, pos int) ast.Expr {
	for i, e := range n.Elts {
		kv, keyed := e.(*ast.KeyValueExpr)
		switch {
		case !keyed && i == pos:
			return e
		case keyed:
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
				return kv.Value
			}
		}
	}
	return nil
}

func tail(args []ast.Expr, from int) []ast.Expr {
	if len(args) <= from {
		return nil
	}
	return args[from:]
}

func isString(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
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

// expandedSlice reports what GORM expands into a list: a slice or array of
// anything but bytes ([]byte is a bytea value) that is no driver.Valuer
// (store.Int64s, store.Strings and store.Times are).
func expandedSlice(t types.Type) bool {
	if t == nil {
		return false
	}
	var elem types.Type
	switch u := t.Underlying().(type) {
	case *types.Slice:
		elem = u.Elem()
	case *types.Array:
		elem = u.Elem()
	default:
		return false
	}
	if b, ok := elem.Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
		return false
	}
	obj, _, _ := types.LookupFieldOrMethod(t, false, nil, "Value")
	_, valuer := obj.(*types.Func)
	return !valuer
}

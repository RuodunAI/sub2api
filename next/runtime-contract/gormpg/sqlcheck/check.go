// Package sqlcheck is the one implementation of the [raw-sql] and
// [sql-const] source checks that keep SQL text out of the data: the core
// (server/internal/store/gormguard), the gateway (internal/sqlguard), the SDK
// with every plugin (pgdb's TestNoRawSQL) and runtime-contract itself
// (gormpg's helpers) run it over their non-test Go files, each with a Policy
// of its own: the declarations it allows and the helpers of its own that
// take SQL text.
//
//   - [raw-sql] no gorm Raw or Exec, called or as a value, nor a call of a
//     Policy.Text function marked raw, outside Policy.Raw: a statement is
//     built by GORM's models and builder. A Raw or Exec whose text is a
//     constant set operation over builder queries, "(?) UNION ALL (?)" or
//     "? EXCEPT ?" with an optional ORDER BY of column numbers, passes.
//   - [sql-const] the SQL text handed to GORM is built from constants. SQL
//     text is the first argument of Raw, Exec, Where, Not, Or, Having,
//     Order, Group, Joins, InnerJoins, Table and Pluck; every argument of
//     Select and Distinct, a string or a []string (column lists), unless
//     the first is a constant fragment whose ? or @name take the rest; the
//     first inline condition of Find, First, Take, Last, Delete,
//     FirstOrInit and FirstOrCreate; the argument of gorm.Expr; the first
//     string parameter, by signature, of a Policy.Text function; the SQL
//     of a clause.Expr or NamedExpr literal or assignment; and the Table,
//     Name and Alias of a clause.Column or clause.Table literal whose Raw is
//     not the constant false. Such an argument typed as an interface
//     without methods (any) is reported, its text being unknown; and so is
//     a gorm method that takes SQL text used as a value or a method
//     expression, whose arguments are only seen at a call. A method of an interface that is
//     declared as gorm's (Exec(string, ...interface{}) *gorm.DB) counts as
//     gorm's. Built from constants are: constants and + of them; variables,
//     parameters, unexported fields of the package and package functions
//     whose every value is such; elements of a map or slice literal of such
//     that nothing writes; fmt.Sprintf, strings.Join, Repeat and the trims
//     of such; values of a Policy.ConstTypes type, every conversion to which
//     must itself be of such text; the TableName of a Policy.Generated
//     package; and the results of Policy.Quoting functions. A value goes in
//     as a ? parameter; an identifier chosen at run time goes through a
//     fixed map to a clause.Column or clause.OrderByColumn.
//
// Every entry of a Policy must allow or match something: an entry that names
// no declaration, or one that no longer needs it, fails (Unused).
package sqlcheck

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"regexp"
	"sort"
	"strings"
)

// The rule tags, at the start of every finding's message.
const (
	RuleRawSQL   = "[raw-sql]"
	RuleSQLConst = "[sql-const]"
)

const (
	gormPkg   = "gorm.io/gorm"
	clausePkg = "gorm.io/gorm/clause"
)

// Policy is what one module allows and adds. Keys name a declaration as
// "pkg.Name" or "pkg.Type.Method", pkg being the import path relative to
// Module (a path outside it stays whole); a var or const group is named by
// its first name.
type Policy struct {
	// Module is the import path the keys are relative to.
	Module string
	// Raw lists the declarations that may run SQL text written out
	// ([raw-sql]), with the reason GORM's builder cannot express it.
	Raw map[string]string
	// Dynamic lists the declarations whose SQL text may be built from what
	// [sql-const] cannot prove constant, with the reason.
	Dynamic map[string]string
	// Quoting lists the functions whose string result is a quoted
	// identifier or literal, constant text whatever their arguments.
	Quoting map[string]string
	// Text lists the module's own functions and methods that take SQL text:
	// their first string parameter is checked as gorm's are. True marks one
	// that runs the text, a [raw-sql] call as Raw and Exec are.
	Text map[string]bool
	// ConstTypes lists the string types whose values are constant SQL text:
	// every conversion to one must be of constant text.
	ConstTypes map[string]bool
	// Generated lists the packages of generated models, whose TableName
	// methods return a literal.
	Generated map[string]bool
}

// Checker runs the rules of one Policy over packages, and remembers which
// entries allowed or matched something.
type Checker struct {
	p    Policy
	used map[string]bool // "rule key" of Raw and Dynamic, "Quoting key" ...
}

// New returns a Checker for p.
func New(p Policy) *Checker { return &Checker{p: p, used: map[string]bool{}} }

// Unused returns the entries of the policy that allowed or matched nothing,
// sorted, as "list key".
func (c *Checker) Unused() []string {
	var out []string
	lists := map[string][]string{
		RuleRawSQL: keys(c.p.Raw), RuleSQLConst: keys(c.p.Dynamic), "Quoting": keys(c.p.Quoting),
		"Text": keys(c.p.Text), "ConstTypes": keys(c.p.ConstTypes), "Generated": keys(c.p.Generated),
	}
	for list, ks := range lists {
		for _, k := range ks {
			if !c.used[list+" "+k] {
				out = append(out, list+" "+k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// textMethods are the gorm (*DB) methods whose first argument, a string, is
// SQL text (Select and Distinct: every argument, selectArgs).
var textMethods = map[string]bool{
	"Raw": true, "Exec": true, "Where": true, "Not": true, "Or": true, "Having": true, "Select": true,
	"Distinct": true, "Order": true, "Group": true, "Joins": true, "InnerJoins": true, "Table": true, "Pluck": true,
}

// condMethods are the gorm (*DB) finders whose second argument, the first
// inline condition, is SQL text when it is a string.
var condMethods = map[string]bool{
	"Find": true, "First": true, "Take": true, "Last": true, "Delete": true, "FirstOrInit": true, "FirstOrCreate": true,
}

// combinatorText is the text of a set operation over builder queries,
// (q1) UNION ALL (q2) ... [ORDER BY 1], each query in parentheses or not:
// it holds no statement of its own, so [raw-sql] passes a Raw or Exec of
// it.
var combinatorText = regexp.MustCompile(`(?i)^\s*(\(\?\)|\?)(\s+(UNION|INTERSECT|EXCEPT)(\s+ALL)?\s+(\(\?\)|\?))+(\s+ORDER\s+BY\s+\d+(\s*,\s*\d+)*)?\s*$`)

// pass checks one package.
type pass struct {
	*Checker
	fset *token.FileSet
	info *types.Info
	pkg  string // the package, relative to the module
	out  []finding
	// called are the selectors in the function position of a call;
	// combinators the Raw and Exec of a combinator, which [raw-sql] passes.
	called, combinators map[*ast.SelectorExpr]bool
	index
}

// Check returns the findings of the non-test files of one type-checked
// package as "file:line:col: [rule] message", in source order. info must hold Types, Defs,
// Uses and Selections (TypeCheck).
func (c *Checker) Check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) []string {
	p := &pass{Checker: c, fset: fset, info: info, pkg: c.rel(pkgPath),
		called: map[*ast.SelectorExpr]bool{}, combinators: map[*ast.SelectorExpr]bool{}}
	p.indexFiles(files)
	for _, f := range files {
		if strings.HasSuffix(fset.Position(f.Package).Filename, "_test.go") {
			continue
		}
		for _, decl := range f.Decls {
			key := p.pkg + "." + declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					p.call(n, key)
				case *ast.SelectorExpr:
					p.selector(n, key)
				case *ast.CompositeLit:
					p.literal(n, key)
				case *ast.AssignStmt:
					p.assign(n, key)
				}
				return true
			})
		}
	}
	sort.SliceStable(p.out, func(i, j int) bool { return p.out[i].pos < p.out[j].pos })
	out := make([]string, len(p.out))
	for i, f := range p.out {
		out[i] = f.text
	}
	return out
}

// finding is a finding at pos, in source order once sorted.
type finding struct {
	pos  token.Pos
	text string
}

// rel is pkgPath relative to the module.
func (c *Checker) rel(pkgPath string) string { return strings.TrimPrefix(pkgPath, c.p.Module+"/") }

// funcKey names fn as the policy does: pkg.Func or pkg.Type.Method.
func (c *Checker) funcKey(fn *types.Func) string {
	if fn.Pkg() == nil {
		return fn.Name()
	}
	name := fn.Name()
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		if n := namedOf(recv.Type()); n != nil {
			name = n.Obj().Name() + "." + name
		}
	}
	return c.rel(fn.Pkg().Path()) + "." + name
}

// typeKey names a named type as the policy does: pkg.Type.
func (c *Checker) typeKey(t types.Type) string {
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return ""
	}
	return c.rel(n.Obj().Pkg().Path()) + "." + n.Obj().Name()
}

// listed reports whether m, the policy's list named list, holds key, and
// records the use.
func listed[V any](c *Checker, list string, m map[string]V, key string) bool {
	if _, ok := m[key]; !ok {
		return false
	}
	c.used[list+" "+key] = true
	return true
}

// textFunc reports a Policy.Text function and whether it runs its text.
func (c *Checker) textFunc(fn *types.Func) (text, raw bool) {
	key := c.funcKey(fn)
	raw, text = c.p.Text[key]
	if text {
		c.used["Text "+key] = true
	}
	return text, raw
}

// constType reports a Policy.ConstTypes type.
func (c *Checker) constType(t types.Type) bool {
	return t != nil && listed(c, "ConstTypes", c.p.ConstTypes, c.typeKey(t))
}

// report records a finding of rule at n unless list allows key.
func (p *pass) report(n ast.Node, rule, key string, list map[string]string, format string, args ...any) {
	if listed(p.Checker, rule, list, key) {
		return
	}
	p.out = append(p.out, finding{n.Pos(), fmt.Sprintf("%s: %s %s", p.fset.Position(n.Pos()), rule, fmt.Sprintf(format, args...))})
}

func (p *pass) rawFinding(n ast.Node, key, what string) {
	p.report(n, RuleRawSQL, key, p.p.Raw, "%s in %s: a statement written as SQL text; build it with GORM's models and query builder (Where, Updates, clause.OnConflict ...), or list the declaration with the reason GORM cannot express it", what, key)
}

func (p *pass) constFinding(n ast.Node, key, format string, args ...any) {
	p.report(n, RuleSQLConst, key, p.p.Dynamic, format+" (in %s): pass values as ? parameters, and identifiers chosen at run time through a fixed map as clause.Column / clause.OrderByColumn", append(args, key)...)
}

// call checks the SQL text arguments of a call, and marks the selectors of
// calls and of combinators.
func (p *pass) call(n *ast.CallExpr, key string) {
	if tv, ok := p.info.Types[n.Fun]; ok && tv.IsType() {
		if p.constType(tv.Type) && len(n.Args) == 1 && !p.constSQL(n.Args[0]) {
			p.constFinding(n.Args[0], key, "conversion to %s of SQL text not built from constants", tv.Type)
		}
		return
	}
	sel, _ := ast.Unparen(n.Fun).(*ast.SelectorExpr)
	if sel != nil {
		p.called[sel] = true
	}
	fn := calledFunc(p.info, n)
	if fn == nil || sel != nil && p.methodExpr(sel) {
		return
	}
	args := n.Args
	if n.Ellipsis.IsValid() {
		args = args[:len(args)-1] // a spread passes values
	}
	switch gorm := p.gormMethod(fn); {
	case gorm && (fn.Name() == "Select" || fn.Name() == "Distinct"):
		p.selectArgs(args, key, fn.Name())
	case gorm && textMethods[fn.Name()] && len(args) > 0:
		if (fn.Name() == "Raw" || fn.Name() == "Exec") && combinatorText.MatchString(p.constString(args[0])) && sel != nil {
			p.combinators[sel] = true
		}
		p.sqlArg(args[0], key, "(*gorm.DB)."+fn.Name(), false)
	case gorm && condMethods[fn.Name()] && len(args) > 1:
		p.sqlArg(args[1], key, "the inline condition of (*gorm.DB)."+fn.Name(), false)
	case isGormFunc(fn, "Expr") && len(args) > 0:
		p.sqlArg(args[0], key, "gorm.Expr", false)
	default:
		if text, _ := p.textFunc(fn); text {
			if i := firstString(fn); i >= 0 && i < len(args) {
				p.sqlArg(args[i], key, p.funcKey(fn), false)
			}
		}
	}
}

// selectArgs checks the arguments of Select or Distinct: SQL text, a column
// list of strings and []string, unless the first is a constant fragment
// whose ? or @name placeholders take the rest (GORM's rule).
func (p *pass) selectArgs(args []ast.Expr, key, method string) {
	if len(args) == 0 {
		return
	}
	where := "(*gorm.DB)." + method
	p.sqlArg(args[0], key, where, true)
	if s := p.constString(args[0]); len(args) > 1 && (strings.Count(s, "?") >= len(args)-1 || strings.Contains(s, "@")) {
		return
	}
	for _, a := range args[1:] {
		if t := p.info.TypeOf(a); isString(t) || isStringList(t) {
			p.sqlArg(a, key, where, true)
		}
	}
}

// sqlArg reports e, an argument in the place of SQL text, unless it is
// built from constants. lists admits a []string of such (Select's column
// list); an interface value's text is unknown.
func (p *pass) sqlArg(e ast.Expr, key, where string, lists bool) {
	t := p.info.TypeOf(e)
	switch {
	case t == nil:
	case isString(t):
		if !p.constSQL(e) {
			p.constFinding(e, key, "SQL text of %s not built from constants", where)
		}
	case lists && isStringList(t):
		if !p.constList(e) {
			p.constFinding(e, key, "column list of %s not built from constants", where)
		}
	case isEmptyInterface(t):
		p.constFinding(e, key, "%s value in the place of SQL text of %s: it may hold a string whose text cannot be checked; pass the string, struct, map or clause itself", t, where)
	}
}

// selector reports gorm's Raw and Exec and the raw Policy.Text functions,
// called or as values ([raw-sql]), and the functions that take SQL text
// used as values or method expressions ([sql-const]): only a call's
// arguments are checked.
func (p *pass) selector(n *ast.SelectorExpr, key string) {
	fn := funcOf(p.info, n)
	if fn == nil {
		return
	}
	gorm := p.gormMethod(fn)
	text, raw := p.textFunc(fn)
	if (gorm && (fn.Name() == "Raw" || fn.Name() == "Exec") || raw) && !p.combinators[n] {
		p.rawFinding(n, key, fn.Name())
	}
	sqlFunc := gorm && (textMethods[fn.Name()] || condMethods[fn.Name()]) || isGormFunc(fn, "Expr") || text
	if sqlFunc && (!p.called[n] || p.methodExpr(n)) {
		p.constFinding(n, key, "%s used as a value or method expression: its SQL text is checked only in a direct call", fn.Name())
	}
}

// literal checks the SQL of a clause.Expr or NamedExpr literal, and the
// names of a clause.Column or clause.Table literal that writes them raw.
func (p *pass) literal(n *ast.CompositeLit, key string) {
	t := p.info.TypeOf(n)
	switch {
	case isNamed(t, clausePkg, "Expr"), isNamed(t, clausePkg, "NamedExpr"):
		if e := fieldValue(n, "SQL", 0); e != nil {
			p.sqlArg(e, key, t.String()+".SQL", false)
		}
	case isNamed(t, clausePkg, "Column"):
		p.rawNames(n, key, t, fieldValue(n, "Raw", 3), "Table", "Name", "Alias")
	case isNamed(t, clausePkg, "Table"):
		p.rawNames(n, key, t, fieldValue(n, "Raw", 2), "Name", "Alias")
	}
}

// rawNames checks the string fields of a clause.Column or Table literal,
// named in the struct's order, unless raw is absent or the constant false:
// GORM writes them unquoted.
func (p *pass) rawNames(n *ast.CompositeLit, key string, t types.Type, raw ast.Expr, fields ...string) {
	if raw == nil || p.constBool(raw, false) {
		return
	}
	for i, name := range fields { // in the order of the struct, Raw last
		if e := fieldValue(n, name, i); e != nil && !p.constSQL(e) {
			p.constFinding(e, key, "%s of a raw %s not built from constants", name, t)
		}
	}
}

// assign checks what an assignment writes into the SQL of a clause.Expr or
// NamedExpr, and the Raw of a clause.Column or Table, which only a literal
// may set: there its names are checked.
func (p *pass) assign(n *ast.AssignStmt, key string) {
	for i, l := range n.Lhs {
		sel, ok := ast.Unparen(l).(*ast.SelectorExpr)
		if !ok {
			continue
		}
		f := fieldOf(p.info, sel)
		if f == nil {
			continue
		}
		var rhs ast.Expr
		if len(n.Lhs) == len(n.Rhs) {
			rhs = n.Rhs[i]
		}
		recv := p.info.TypeOf(sel.X)
		switch {
		case f.Name() == "SQL" && (isNamed(recv, clausePkg, "Expr") || isNamed(recv, clausePkg, "NamedExpr")):
			if rhs == nil || !p.constSQL(rhs) {
				p.constFinding(l, key, "SQL of %s assigned text not built from constants", recv)
			}
		case f.Name() == "Raw" && (isNamed(recv, clausePkg, "Column") || isNamed(recv, clausePkg, "Table")):
			if rhs == nil || !p.constBool(rhs, false) {
				p.constFinding(l, key, "Raw of %s set outside a literal: write the literal, with Raw and constant names", recv)
			}
		}
	}
}

// gormMethod reports a method of gorm.DB (one promoted through an embedded
// *gorm.DB included), or a method of an interface declared as gorm.DB's
// (same name and signature), which a *gorm.DB may stand behind.
func (p *pass) gormMethod(fn *types.Func) bool {
	sig := fn.Type().(*types.Signature)
	recv := sig.Recv()
	if recv == nil {
		return false
	}
	if isNamed(recv.Type(), gormPkg, "DB") {
		return true
	}
	if !types.IsInterface(recv.Type()) || sig.Results().Len() != 1 || !isNamed(sig.Results().At(0).Type(), gormPkg, "DB") {
		return false
	}
	db := namedOf(sig.Results().At(0).Type())
	obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(db), false, db.Obj().Pkg(), fn.Name())
	m, ok := obj.(*types.Func)
	return ok && types.Identical(withoutRecv(m.Type().(*types.Signature)), withoutRecv(sig))
}

func withoutRecv(s *types.Signature) *types.Signature {
	return types.NewSignatureType(nil, nil, nil, s.Params(), s.Results(), s.Variadic())
}

// methodExpr reports a method expression, (*gorm.DB).Where.
func (p *pass) methodExpr(sel *ast.SelectorExpr) bool {
	s := p.info.Selections[sel]
	return s != nil && s.Kind() == types.MethodExpr
}

// constString is the value of e when it is a constant string, else "".
func (p *pass) constString(e ast.Expr) string {
	if tv, ok := p.info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return constant.StringVal(tv.Value)
	}
	return ""
}

// constBool reports whether e is the boolean constant want.
func (p *pass) constBool(e ast.Expr, want bool) bool {
	tv, ok := p.info.Types[e]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.Bool && constant.BoolVal(tv.Value) == want
}

// firstString is the index of fn's first string parameter, or -1.
func firstString(fn *types.Func) int {
	params := fn.Type().(*types.Signature).Params()
	for i := range params.Len() {
		if isString(params.At(i).Type()) {
			return i
		}
	}
	return -1
}

// declName names a top-level declaration: a function, Type.Method, or the
// first name of a var or const spec (of the first spec of a group).
func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil || len(d.Recv.List) == 0 {
			return d.Name.Name
		}
		t := d.Recv.List[0].Type
		for {
			switch x := t.(type) {
			case *ast.StarExpr:
				t = x.X
				continue
			case *ast.IndexExpr:
				t = x.X
				continue
			case *ast.IndexListExpr:
				t = x.X
				continue
			case *ast.Ident:
				return x.Name + "." + d.Name.Name
			}
			return d.Name.Name
		}
	case *ast.GenDecl:
		for _, s := range d.Specs {
			if v, ok := s.(*ast.ValueSpec); ok && len(v.Names) > 0 {
				return v.Names[0].Name
			}
		}
	}
	return ""
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

// fieldOf is the struct field a selector reads or writes, if any.
func fieldOf(info *types.Info, sel *ast.SelectorExpr) *types.Var {
	if s := info.Selections[sel]; s != nil && s.Kind() == types.FieldVal {
		v, _ := s.Obj().(*types.Var)
		return v
	}
	return nil
}

// funcOf is the function or method a selector names, if any.
func funcOf(info *types.Info, sel *ast.SelectorExpr) *types.Func {
	if s := info.Selections[sel]; s != nil {
		fn, _ := s.Obj().(*types.Func)
		return fn
	}
	fn, _ := info.Uses[sel.Sel].(*types.Func) // a qualified identifier: gorm.Expr
	return fn
}

// calledFunc is the function or method a call names, if any.
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		return funcOf(info, f)
	case *ast.IndexExpr: // an instantiation, f[T](...)
		return calledFunc(info, &ast.CallExpr{Fun: f.X})
	}
	return nil
}

func isGormFunc(fn *types.Func, name string) bool {
	return fn.Pkg() != nil && fn.Pkg().Path() == gormPkg && fn.Name() == name && fn.Type().(*types.Signature).Recv() == nil
}

// namedOf is t, or what it points to, as a named type, else nil.
func namedOf(t types.Type) *types.Named {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, _ := types.Unalias(t).(*types.Named)
	return n
}

func isNamed(t types.Type, pkg, name string) bool {
	n := namedOf(t)
	return n != nil && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

func isString(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// isEmptyInterface reports an interface without methods (any), which may
// hold a string; one with methods (clause.Expression) cannot.
func isEmptyInterface(t types.Type) bool {
	i, ok := t.Underlying().(*types.Interface)
	return ok && i.Empty()
}

// isStringList reports a slice or array of strings.
func isStringList(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return isString(u.Elem())
	case *types.Array:
		return isString(u.Elem())
	}
	return false
}

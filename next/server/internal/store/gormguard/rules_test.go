package gormguard

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

// Rule tags, at the start of every finding.
const (
	ruleTx       = "[gorm-tx]"
	ruleOpen     = "[gorm-open]"
	ruleInstance = "[gorm-instance]"
	ruleSlice    = "[gorm-slice]"
	rulePgx      = "[pgx-direct]"
	ruleUnrun    = "[gorm-raw-unrun]"
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

// runMethods are the gorm (*DB) methods that run the statement built so
// far; Raw only builds one.
var runMethods = map[string]bool{
	"Scan": true, "Row": true, "Rows": true, "ScanRows": true, "Pluck": true, "Count": true,
	"Find": true, "FindInBatches": true, "First": true, "Take": true, "Last": true, "FirstOrInit": true, "FirstOrCreate": true,
	"Exec": true, "Create": true, "CreateInBatches": true, "Save": true, "Update": true, "Updates": true,
	"UpdateColumn": true, "UpdateColumns": true, "Delete": true,
}

// pgxMethods are the direct statement calls of a pgx querier.
var pgxMethods = map[string]bool{"Exec": true, "Query": true, "QueryRow": true}

type rules struct {
	module   string
	owners   map[string]string
	migrated map[string]string // packages, relative to the module
	files    map[string]string // single migrated files, relative to the module
}

// check returns the findings of one type-checked package as
// "file:line:col: [rule] message". The files must be parsed with comments,
// and info must hold Defs as well as Types, Uses and Selections.
func (r rules) check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) []string {
	rel := strings.TrimPrefix(pkgPath, r.module+"/")
	_, owner := r.owners[rel]
	_, migrated := r.migrated[rel]
	c := &checker{fset: fset, info: info, owner: owner, storePkg: r.module + "/internal/store"}
	c.index(files)
	for _, f := range files {
		c.allowed = allowComments(fset, f)
		_, file := r.files[rel+"/"+filepath.Base(fset.Position(f.Package).Filename)]
		c.migrated = migrated || file
		ast.Inspect(f, c.visit)
	}
	return c.out
}

type checker struct {
	fset            *token.FileSet
	info            *types.Info
	owner, migrated bool
	storePkg        string         // import path of package store
	allowed         map[int]string // line -> reason of an allow-pgx comment
	out             []string

	// What [gorm-raw-unrun] follows a Raw statement through: the functions
	// of the package, the assignments and uses of its local variables, and
	// whether a function returns an unrun Raw statement (memoized).
	funcs   map[*types.Func]*ast.FuncDecl
	assigns map[*types.Var][]assignment
	uses    map[*types.Var][]use
	rawFns  map[*types.Func]bool
}

// assignment is a value given to a local variable as of position at: rhs,
// or nil when it is no single expression (v, err := f(), a range).
type assignment struct {
	at  token.Pos
	rhs ast.Expr
}

// use is a mention of a local variable other than a := declaration;
// fieldRead marks a field read such as q.Error, which runs nothing.
type use struct {
	pos       token.Pos
	fieldRead bool
}

// index records the functions of the package and the assignments and uses
// of its local variables, for unrunRaw.
func (c *checker) index(files []*ast.File) {
	c.funcs = map[*types.Func]*ast.FuncDecl{}
	c.assigns = map[*types.Var][]assignment{}
	c.uses = map[*types.Var][]use{}
	c.rawFns = map[*types.Func]bool{}
	fieldReads := map[*ast.Ident]bool{}
	assign := func(at token.Pos, lhs []ast.Expr, rhs []ast.Expr) {
		for i, l := range lhs {
			id, _ := l.(*ast.Ident)
			v := c.localVar(id)
			if v == nil {
				continue
			}
			a := assignment{at: at}
			if len(lhs) == len(rhs) {
				a.rhs = rhs[i]
			}
			c.assigns[v] = append(c.assigns[v], a)
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if fn, ok := c.info.Defs[n.Name].(*types.Func); ok {
					c.funcs[fn] = n
				}
			case *ast.AssignStmt:
				assign(n.End(), n.Lhs, n.Rhs)
			case *ast.RangeStmt:
				assign(n.X.End(), []ast.Expr{n.Key, n.Value}, nil)
			case *ast.ValueSpec:
				lhs := make([]ast.Expr, len(n.Names))
				for i, id := range n.Names {
					lhs[i] = id
				}
				assign(n.End(), lhs, n.Values)
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok {
					if s := c.info.Selections[n]; s != nil && s.Kind() == types.FieldVal {
						fieldReads[id] = true
					}
				}
			}
			return true
		})
	}
	for id := range c.info.Uses {
		if v := c.localVar(id); v != nil {
			c.uses[v] = append(c.uses[v], use{pos: id.Pos(), fieldRead: fieldReads[id]})
		}
	}
}

// localVar is the variable, declared in a function, that id names.
func (c *checker) localVar(id *ast.Ident) *types.Var {
	if id == nil {
		return nil
	}
	obj := c.info.Defs[id]
	if obj == nil {
		obj = c.info.Uses[id]
	}
	v, _ := obj.(*types.Var)
	if v == nil || v.IsField() || v.Pkg() == nil || v.Parent() == nil || v.Parent() == v.Pkg().Scope() {
		return nil
	}
	return v
}

func (c *checker) report(n ast.Node, format string, args ...any) {
	c.out = append(c.out, fmt.Sprintf("%s: %s", c.fset.Position(n.Pos()), fmt.Sprintf(format, args...)))
}

func (c *checker) visit(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.SelectorExpr:
		c.selector(n)
		c.unrun(n)
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

// unrun reports the Error or RowsAffected of a Raw statement that no
// method ran (unrunRaw): db.Raw(sql).Error is nil without the statement
// ever reaching the database. A write takes Exec instead of Raw, a read
// ends in Scan, Row or Rows.
func (c *checker) unrun(n *ast.SelectorExpr) {
	if n.Sel.Name != "Error" && n.Sel.Name != "RowsAffected" {
		return
	}
	if s := c.info.Selections[n]; s == nil || s.Kind() != types.FieldVal || s.Obj().Pkg() == nil || s.Obj().Pkg().Path() != gormPkg {
		return
	}
	if c.unrunRaw(n.X) {
		c.report(n, "%s %s of a Raw statement that never ran: use Exec for a write, end a read in Scan, Row or Rows", ruleUnrun, n.Sel.Name)
	}
}

// unrunRaw reports whether e is a Raw statement that no method ran: a
// chain of gorm methods that reaches Raw without a run method (Scan, Exec
// ...), seen through parentheses, through a local variable whose last
// assignment is such a statement (varHoldsRaw), and through a call of a
// function of the package that returns one (returnsRaw).
func (c *checker) unrunRaw(e ast.Expr) bool {
	for {
		switch x := ast.Unparen(e).(type) {
		case *ast.Ident:
			return c.varHoldsRaw(x)
		case *ast.CallExpr:
			fn := calledFunc(c.info, x)
			switch {
			case fn == nil:
				return false
			case !isGormMethod(fn):
				return c.returnsRaw(fn)
			case runMethods[fn.Name()]:
				return false
			case fn.Name() == "Raw":
				return true
			}
			sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr)
			if !ok {
				return false
			}
			e = sel.X
		default:
			return false
		}
	}
}

// varHoldsRaw reports whether the local variable id names holds an unrun
// Raw statement where id stands: its last assignment before id is one, and
// nothing mentions the variable in between but field reads. Any other
// mention may have run the statement, which the variable shares: q.Scan,
// a chain built on q, a call or a closure that takes q. Branches and loops
// are not followed: the last assignment is the last one in the text.
func (c *checker) varHoldsRaw(id *ast.Ident) bool {
	v := c.localVar(id)
	if v == nil {
		return false
	}
	var last *assignment
	for i, a := range c.assigns[v] {
		if a.at <= id.Pos() && (last == nil || a.at > last.at) {
			last = &c.assigns[v][i]
		}
	}
	if last == nil || last.rhs == nil {
		return false
	}
	for _, u := range c.uses[v] {
		if !u.fieldRead && u.pos > last.at && u.pos < id.Pos() {
			return false
		}
	}
	return c.unrunRaw(last.rhs)
}

// returnsRaw reports whether fn is a function of the package that returns
// a single *gorm.DB, and an unrun Raw statement from every return.
func (c *checker) returnsRaw(fn *types.Func) bool {
	fn = fn.Origin()
	if raw, ok := c.rawFns[fn]; ok {
		return raw
	}
	c.rawFns[fn] = false // while it is being looked at: recursion returns no Raw statement of its own
	decl := c.funcs[fn]
	res := fn.Type().(*types.Signature).Results()
	if decl == nil || decl.Body == nil || res.Len() != 1 || !isNamed(res.At(0).Type(), gormPkg, "DB") {
		return false
	}
	found, all := false, true
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			found = true
			all = all && len(n.Results) == 1 && c.unrunRaw(n.Results[0])
		}
		return all
	})
	c.rawFns[fn] = found && all
	return c.rawFns[fn]
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
		c.report(n, "%s pgx %s in a migrated package or file: build the statement with store.G (Batch and CopyFrom stay on pgx; a measured hot path takes %s <reason>)",
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
	if i, ok := c.sqlIndex(fn, n); ok && i < len(n.Args) {
		vars := n.Args[i+1:]
		if n.Ellipsis.IsValid() {
			vars = vars[:len(vars)-1]
		}
		c.checkParenBytes(n.Args[i], vars, false, "gorm "+fn.Name())
	}
}

// sqlIndex is the index of the argument of a gorm call that is SQL text
// whose ? placeholders take the arguments after it: the condition or
// statement of Where, Or, Not, Having, Raw, Exec and Joins, the inline
// condition of the finders, a fragment given to Select, and the text of
// gorm.Expr and store.Conds.And.
func (c *checker) sqlIndex(fn *types.Func, n *ast.CallExpr) (int, bool) {
	switch {
	case isGormFunc(fn, "Expr"), fn.Name() == "And" && isMethodOf(fn, c.storePkg, "Conds"):
		return 0, true
	case !isGormMethod(fn):
		return 0, false
	case fn.Name() == "Select":
		return 0, len(n.Args) > 0 && isString(c.info.TypeOf(n.Args[0]))
	case fn.Name() == "Assign", fn.Name() == "Attrs", fn.Name() == "Update", fn.Name() == "UpdateColumn":
		return 0, false // values, not SQL text
	}
	i, ok := conditionMethods[fn.Name()]
	return i, ok
}

// checkParenBytes reports a byte slice bound to a ? that directly follows
// "(" in sqlArg, when sqlArg is a constant SQL text whose placeholders
// take vars in order (all: at every ?, clause.Expr's WithoutParentheses).
// There clause.Expr.Build expands any slice that is no driver.Valuer,
// []byte and json.RawMessage included, into one parameter per element, and
// an empty one into NULL.
func (c *checker) checkParenBytes(sqlArg ast.Expr, vars []ast.Expr, all bool, where string) {
	tv, ok := c.info.Types[sqlArg]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return
	}
	for i, paren := range parenPlaceholders(constant.StringVal(tv.Value), len(vars)) {
		if t := c.info.TypeOf(vars[i]); (paren || all) && byteSlice(t) {
			c.report(vars[i], `%s %s argument to %s at a "(?": GORM binds each byte as a parameter there (an empty one as NULL); wrap it in store.Bytes, or bind it through gorm.Expr("?", v)`, ruleSlice, t, where)
		}
	}
}

// parenPlaceholders reports, for each of n arguments, whether the ? that
// takes it directly follows "(" in sql, as clause.Expr.Build reads the
// text (a ? past the last argument is text).
func parenPlaceholders(sql string, n int) []bool {
	out := make([]bool, n)
	idx, afterParen := 0, false
	for i := 0; i < len(sql); i++ {
		if sql[i] == '?' && idx < n {
			out[idx] = afterParen
			idx++
			continue
		}
		afterParen = sql[i] == '('
	}
	return out
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
	case fn.Name() == "And" && isMethodOf(fn, c.storePkg, "Conds"):
		// store.Conds.And passes its arguments to Where.
		return tail(args, 1)
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
	all := false
	if wp, ok := c.info.Types[fieldValue(n, "WithoutParentheses", 2)]; ok && wp.Value != nil {
		all = constant.BoolVal(wp.Value)
	}
	c.checkParenBytes(fieldValue(n, "SQL", 0), vars.Elts, all, t.String())
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
func isGormMethod(fn *types.Func) bool { return isMethodOf(fn, gormPkg, "DB") }

// isMethodOf reports a method of the named type pkg.name.
func isMethodOf(fn *types.Func, pkg, name string) bool {
	recv := fn.Type().(*types.Signature).Recv()
	return recv != nil && isNamed(recv.Type(), pkg, name)
}

// calledFunc is the function or method a call names, if any.
func calledFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		return funcOf(info, f)
	}
	return nil
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

// expandedSlice reports what GORM expands into a list wherever it is
// bound: a slice or array of anything but bytes that is no driver.Valuer
// (store.Int64s, store.Strings and store.Times are). A byte slice is one
// bytea value except at a "(?" (byteSlice, checkParenBytes).
func expandedSlice(t types.Type) bool {
	elem := sliceElem(t)
	if elem == nil || isByte(elem) {
		return false
	}
	return !isValuer(t)
}

// byteSlice reports a slice or array of bytes ([]byte, json.RawMessage)
// that is no driver.Valuer.
func byteSlice(t types.Type) bool {
	elem := sliceElem(t)
	return elem != nil && isByte(elem) && !isValuer(t)
}

// sliceElem is the element type of a slice or array type, else nil.
func sliceElem(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	switch u := t.Underlying().(type) {
	case *types.Slice:
		return u.Elem()
	case *types.Array:
		return u.Elem()
	}
	return nil
}

func isByte(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Byte
}

// isValuer reports a type with a Value method, a driver.Valuer: GORM binds
// it as one value anywhere.
func isValuer(t types.Type) bool {
	obj, _, _ := types.LookupFieldOrMethod(t, false, nil, "Value")
	_, ok := obj.(*types.Func)
	return ok
}

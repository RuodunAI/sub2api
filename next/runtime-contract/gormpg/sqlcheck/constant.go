package sqlcheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// index is what constSQL follows a string through: the values given to
// each variable and field of the package (nil: unknown), the declarations
// and calls of its functions, and whether a function is used other than
// called.
type index struct {
	funcs   map[*types.Func]*ast.FuncDecl
	values  map[*types.Var][]ast.Expr
	ranges  map[*types.Var][]ast.Expr // ranged over: the value is an element of the expression
	results map[*types.Var][]result   // a, b := f(): a result of a call
	calls   map[*types.Func][]*ast.CallExpr
	escapes map[*types.Func]bool
	// memo and the parameters bound while a callee's results are looked at.
	varMemo  map[*types.Var]int8
	funcMemo map[string]int8
	bound    map[*types.Var]bool
}

// result is result i of call.
type result struct {
	call *ast.CallExpr
	i    int
}

const (
	memoVisiting int8 = iota + 1
	memoYes
	memoNo
)

// indexFiles records, for constSQL, every value given to a variable or field
// of the package and every declaration and call of its functions.
func (p *pass) indexFiles(files []*ast.File) {
	x := &p.index
	*x = index{funcs: map[*types.Func]*ast.FuncDecl{}, values: map[*types.Var][]ast.Expr{}, ranges: map[*types.Var][]ast.Expr{},
		results: map[*types.Var][]result{}, calls: map[*types.Func][]*ast.CallExpr{}, escapes: map[*types.Func]bool{},
		varMemo: map[*types.Var]int8{}, funcMemo: map[string]int8{}, bound: map[*types.Var]bool{}}
	called := map[*ast.Ident]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if fn, ok := p.info.Defs[n.Name].(*types.Func); ok {
					x.funcs[fn] = n
				}
			case *ast.AssignStmt:
				p.indexAssign(n)
			case *ast.ValueSpec:
				for i, id := range n.Names {
					v := p.varOf(id)
					switch {
					case v == nil:
					case len(n.Values) == 0:
						// the zero value, "" for a string
					case len(n.Values) == len(n.Names):
						x.values[v] = append(x.values[v], n.Values[i])
					default:
						p.indexResult(v, n.Values[0], i)
					}
				}
			case *ast.RangeStmt:
				if v := p.varOf(n.Value); v != nil {
					x.ranges[v] = append(x.ranges[v], n.X)
				}
			case *ast.CompositeLit:
				st, ok := p.info.TypeOf(n).Underlying().(*types.Struct)
				for i, el := range n.Elts {
					if !ok {
						break
					}
					if kv, keyed := el.(*ast.KeyValueExpr); keyed {
						if id, isID := kv.Key.(*ast.Ident); isID {
							if f, isVar := p.info.Uses[id].(*types.Var); isVar && f.IsField() {
								x.values[f] = append(x.values[f], kv.Value)
							}
						}
					} else if i < st.NumFields() {
						x.values[st.Field(i)] = append(x.values[st.Field(i)], el)
					}
				}
			case *ast.UnaryExpr:
				if sel, isSel := ast.Unparen(n.X).(*ast.SelectorExpr); isSel && n.Op == token.AND {
					if f := fieldOf(p.info, sel); f != nil {
						x.values[f] = append(x.values[f], nil)
					}
				}
				if v := p.varOf(n.X); v != nil && n.Op == token.AND {
					x.values[v] = append(x.values[v], nil) // address taken: written through a pointer
				}
			case *ast.CallExpr:
				var id *ast.Ident
				switch fun := ast.Unparen(n.Fun).(type) {
				case *ast.Ident:
					id = fun
				case *ast.SelectorExpr:
					id = fun.Sel
				}
				if fn, ok := p.info.Uses[id].(*types.Func); ok && id != nil {
					called[id] = true
					x.calls[fn.Origin()] = append(x.calls[fn.Origin()], n)
				}
				for i, a := range n.Args {
					if v := p.varOf(a); v != nil && isList(v.Type()) && p.writesArg(n, i) {
						x.values[v] = append(x.values[v], nil) // handed over: the callee may write it
					}
				}
			}
			return true
		})
	}
	for id, obj := range p.info.Uses {
		if fn, ok := obj.(*types.Func); ok && !called[id] {
			x.escapes[fn.Origin()] = true
		}
	}
}

// indexAssign records the values an assignment gives.
func (p *pass) indexAssign(n *ast.AssignStmt) {
	x := &p.index
	for i, l := range n.Lhs {
		if ix, isIndex := ast.Unparen(l).(*ast.IndexExpr); isIndex {
			if v := p.varOf(ix.X); v != nil {
				x.values[v] = append(x.values[v], nil) // an element written: the list holds an unknown value
			}
			continue
		}
		v := p.varOf(l)
		if sel, isSel := ast.Unparen(l).(*ast.SelectorExpr); isSel {
			v = fieldOf(p.info, sel)
		}
		if v == nil {
			continue
		}
		switch {
		case len(n.Lhs) == len(n.Rhs) && (n.Tok == token.ASSIGN || n.Tok == token.DEFINE || n.Tok == token.ADD_ASSIGN):
			x.values[v] = append(x.values[v], n.Rhs[i])
		case len(n.Rhs) == 1 && i == 0 && isIndexOrAssert(n.Rhs[0]):
			x.values[v] = append(x.values[v], n.Rhs[0]) // v, ok := m[k]
		case len(n.Rhs) == 1:
			p.indexResult(v, n.Rhs[0], i)
		default:
			x.values[v] = append(x.values[v], nil)
		}
	}
}

// indexResult records that v holds result i of e, a call, or an unknown value.
func (p *pass) indexResult(v *types.Var, e ast.Expr, i int) {
	if call, ok := ast.Unparen(e).(*ast.CallExpr); ok {
		p.results[v] = append(p.results[v], result{call, i})
		return
	}
	p.values[v] = append(p.values[v], nil)
}

func isIndexOrAssert(e ast.Expr) bool {
	switch ast.Unparen(e).(type) {
	case *ast.IndexExpr, *ast.TypeAssertExpr:
		return true
	}
	return false
}

// varOf is the variable an identifier declares or uses, if any.
func (p *pass) varOf(e ast.Expr) *types.Var {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	obj := p.info.Defs[id]
	if obj == nil {
		obj = p.info.Uses[id]
	}
	v, _ := obj.(*types.Var)
	return v
}

// constSQL reports whether e, a string, is built from constants alone: a
// constant; a value of a Policy.ConstTypes type; + of such; a variable every
// value of which is such (a range over a list of them, a result of a call
// included); a parameter of a function of the package that every call
// passes such (or, while a call's results are looked at, the call's
// argument); an unexported field of the package every value of which is
// such; a call of a function of the package whose every result is such;
// fmt.Sprintf, strings.Join, Repeat and the trims of such; a conversion of
// such; a quoting helper (Policy.Quoting); the TableName of a generated
// model; an element of a map or slice literal of such that nothing writes.
func (p *pass) constSQL(e ast.Expr) bool {
	e = ast.Unparen(e)
	if tv, ok := p.info.Types[e]; ok && (tv.Value != nil || p.constType(tv.Type)) {
		return true
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		return x.Op == token.ADD && p.constSQL(x.X) && p.constSQL(x.Y)
	case *ast.Ident:
		v, _ := p.info.Uses[x].(*types.Var)
		return v != nil && p.constVar(v, p.constSQL)
	case *ast.CallExpr:
		return p.constCall(x)
	case *ast.SelectorExpr:
		return p.constField(x, p.constSQL)
	case *ast.IndexExpr:
		// An element of a map or slice whose every value is a literal of
		// constants and no element of which is written.
		return p.constList(x.X)
	}
	return false
}

// constList reports a list of strings built from constants: a literal of
// such, a variable or field whose every value is one, append and
// slices.Concat of such, a call of a function of the package whose every
// result is one.
func (p *pass) constList(e ast.Expr) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if !p.constSQL(el) {
				return false
			}
		}
		return true
	case *ast.Ident:
		v, _ := p.info.Uses[x].(*types.Var)
		return v != nil && isList(v.Type()) && p.constVar(v, p.constList)
	case *ast.SelectorExpr:
		return p.constField(x, p.constList)
	case *ast.CallExpr:
		return p.constListCall(x)
	}
	return false
}

// constListCall reports append, slices.Concat or a function of the package
// that builds a list from constants.
func (p *pass) constListCall(call *ast.CallExpr) bool {
	if id, ok := ast.Unparen(call.Fun).(*ast.Ident); ok && id.Name == "append" && len(call.Args) > 0 {
		if _, builtin := p.info.Uses[id].(*types.Builtin); builtin {
			if !p.constList(call.Args[0]) {
				return false
			}
			for i, a := range call.Args[1:] {
				if call.Ellipsis.IsValid() && i == len(call.Args)-2 {
					return p.constList(a)
				}
				if !p.constSQL(a) {
					return false
				}
			}
			return true
		}
	}
	fn := calledFunc(p.info, call)
	if fn == nil {
		return false
	}
	if pkgPath(fn) == "slices" && fn.Name() == "Concat" {
		for _, a := range call.Args {
			if !p.constList(a) {
				return false
			}
		}
		return true
	}
	return !call.Ellipsis.IsValid() && p.constResult(fn, call, 0, p.constList)
}

// constVar reports whether every value of v satisfies ok.
func (p *pass) constVar(v *types.Var, ok func(ast.Expr) bool) bool {
	x := &p.index
	if b, bound := x.bound[v]; bound {
		return b
	}
	switch x.varMemo[v] {
	case memoVisiting, memoYes:
		return true // a cycle adds no value of its own
	case memoNo:
		return false
	}
	x.varMemo[v] = memoVisiting
	res := true
	if p.isParam(v) {
		res = p.constParam(v)
	}
	for _, val := range x.values[v] {
		res = res && val != nil && ok(val)
	}
	for _, r := range x.ranges[v] {
		res = res && p.constList(r)
	}
	for _, r := range x.results[v] {
		fn := calledFunc(p.info, r.call)
		res = res && fn != nil && !r.call.Ellipsis.IsValid() && p.constResult(fn, r.call, r.i, ok)
	}
	if res && len(x.values[v]) == 0 && len(x.ranges[v]) == 0 && len(x.results[v]) == 0 && !p.isParam(v) && !p.declaredHere(v) && !v.IsField() {
		res = false // declared elsewhere (another package): unknown
	}
	if res {
		x.varMemo[v] = memoYes
	} else {
		x.varMemo[v] = memoNo
	}
	return res
}

// constField reports a field read x.f whose every value satisfies ok: an
// unexported field of the package, every value a literal or an assignment
// gives it in the package being one; or the SQL of a clause.Expr or
// NamedExpr, which every literal and assignment of them must give constant
// text (gorm.Expr's argument too).
func (p *pass) constField(sel *ast.SelectorExpr, ok func(ast.Expr) bool) bool {
	v := fieldOf(p.info, sel)
	if v == nil {
		return false
	}
	if s := p.info.Selections[sel]; v.Name() == "SQL" && (isNamed(s.Recv(), clausePkg, "Expr") || isNamed(s.Recv(), clausePkg, "NamedExpr")) {
		return true
	}
	// Only an unexported field: decoders (encoding/json, GORM's Scan) set
	// exported ones by reflection, past every write the index sees.
	return !v.Exported() && v.Pkg() != nil && p.rel(v.Pkg().Path()) == p.pkg && p.constVar(v, ok)
}

// declaredHere reports a variable declared in the checked package.
func (p *pass) declaredHere(v *types.Var) bool {
	return v.Pkg() != nil && p.rel(v.Pkg().Path()) == p.pkg && !v.IsField()
}

// isParam reports a parameter of a function declared in the package.
func (p *pass) isParam(v *types.Var) bool { return p.paramFunc(v) != nil }

func (p *pass) paramFunc(v *types.Var) *ast.FuncDecl {
	for _, decl := range p.funcs {
		if decl.Type.Params != nil && decl.Type.Params.Pos() <= v.Pos() && v.Pos() < decl.Type.Params.End() {
			return decl
		}
	}
	return nil
}

// constParam reports a parameter of a function of the package that is only
// ever called, and called with constant text at its position.
func (p *pass) constParam(v *types.Var) bool {
	decl := p.paramFunc(v)
	fn, _ := p.info.Defs[decl.Name].(*types.Func)
	if fn == nil || p.escapes[fn] {
		return false
	}
	idx := paramIndex(p.info, decl, v)
	if idx < 0 {
		return false
	}
	variadic := fn.Type().(*types.Signature).Variadic() && idx == decl.Type.Params.NumFields()-1
	ok := p.constSQL
	if isList(v.Type()) {
		ok = p.constList
	}
	calls := p.calls[fn]
	for _, call := range calls {
		if variadic && !call.Ellipsis.IsValid() { // f(a, "x", "y"); f(a, list...) passes the list
			if !isString(v.Type().(*types.Slice).Elem()) {
				return false
			}
			for _, a := range call.Args[idx:] {
				if !p.constSQL(a) {
					return false
				}
			}
			continue
		}
		if idx >= len(call.Args) || !ok(call.Args[idx]) { // a spread only passes the variadic parameter
			return false
		}
	}
	return len(calls) > 0
}

// paramIndex is the position of v among decl's parameters.
func paramIndex(info *types.Info, decl *ast.FuncDecl, v *types.Var) int {
	i := 0
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			if info.Defs[name] == v {
				return i
			}
			i++
		}
	}
	return -1
}

// constCall reports a call whose string result is built from constants.
func (p *pass) constCall(call *ast.CallExpr) bool {
	if tv, ok := p.info.Types[call.Fun]; ok && tv.IsType() {
		return len(call.Args) == 1 && p.constSQL(call.Args[0]) // a conversion
	}
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		return len(call.Args) == 0 && p.constReturns(lit.Body, 0, p.constSQL)
	}
	fn := calledFunc(p.info, call)
	if fn == nil || call.Ellipsis.IsValid() && !(pkgPath(fn) == "strings" && fn.Name() == "Join") {
		return false
	}
	if listed(p.Checker, "Quoting", p.p.Quoting, p.funcKey(fn)) {
		return true
	}
	switch path := pkgPath(fn); {
	case fn.Name() == "TableName" && listed(p.Checker, "Generated", p.p.Generated, p.rel(path)):
		return true // generated: the table's name as a literal
	case path == "fmt" && fn.Name() == "Sprintf", path == "strings" && trimFuncs[fn.Name()]:
		for _, a := range call.Args {
			if !p.constSQL(a) {
				return false
			}
		}
		return true
	case path == "strings" && fn.Name() == "Join":
		return len(call.Args) == 2 && p.constList(call.Args[0]) && p.constSQL(call.Args[1])
	case path == "strings" && fn.Name() == "Repeat":
		return len(call.Args) == 2 && p.constSQL(call.Args[0]) // the count only repeats constant text
	}
	return p.constResult(fn, call, 0, p.constSQL)
}

// trimFuncs are the strings functions whose result is cut from their
// arguments.
var trimFuncs = map[string]bool{
	"TrimSpace": true, "TrimSuffix": true, "TrimPrefix": true, "Trim": true, "TrimRight": true, "TrimLeft": true,
	"ToUpper": true, "ToLower": true, "ReplaceAll": true, "Replace": true,
}

func pkgPath(fn *types.Func) string {
	if fn.Pkg() == nil {
		return ""
	}
	return fn.Pkg().Path()
}

// constResult reports a function of the package whose result i satisfies ok
// at every return, its string parameters standing for the call's
// arguments.
func (p *pass) constResult(fn *types.Func, call *ast.CallExpr, i int, ok func(ast.Expr) bool) bool {
	fn = fn.Origin()
	decl := p.funcs[fn]
	if decl == nil || decl.Body == nil || decl.Type.Results == nil || i >= decl.Type.Results.NumFields() {
		return false
	}
	// The arguments, as the parameters see them.
	bound := map[*types.Var]bool{}
	var sig strings.Builder
	sig.WriteString(fn.FullName() + "#" + strconv.Itoa(i)) // result i's type says what ok is
	n := 0
	for _, field := range decl.Type.Params.List {
		for _, name := range field.Names {
			if v, isVar := p.info.Defs[name].(*types.Var); isVar && n < len(call.Args) && isString(v.Type()) {
				bound[v] = p.constSQL(call.Args[n])
				if bound[v] {
					sig.WriteString("+")
				} else {
					sig.WriteString("-")
				}
			}
			n++
		}
		if len(field.Names) == 0 {
			n++
		}
	}
	key := sig.String()
	switch p.funcMemo[key] {
	case memoVisiting, memoYes:
		return true
	case memoNo:
		return false
	}
	p.funcMemo[key] = memoVisiting
	saved, savedMemo := p.bound, p.varMemo
	p.bound, p.varMemo = bound, map[*types.Var]int8{} // what a local holds depends on the bound parameters
	res := p.constReturns(decl.Body, i, ok)
	p.bound, p.varMemo = saved, savedMemo
	if res {
		p.funcMemo[key] = memoYes
	} else {
		p.funcMemo[key] = memoNo
	}
	return res
}

// constReturns reports a function body each of whose returns gives a result
// i that satisfies ok.
func (p *pass) constReturns(body *ast.BlockStmt, i int, ok func(ast.Expr) bool) bool {
	res := true
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			res = res && i < len(n.Results) && ok(n.Results[i])
		}
		return res
	})
	return res
}

// writesArg reports whether call may write the elements of its argument i:
// any function may but gorm's that take SQL text and values (Select's
// column list), those of fmt and strings and the readers of slices; of the
// builtins only copy, delete and clear the first.
func (p *pass) writesArg(call *ast.CallExpr, i int) bool {
	id, _ := ast.Unparen(call.Fun).(*ast.Ident)
	if b, ok := p.info.Uses[id].(*types.Builtin); ok && id != nil {
		return i == 0 && (b.Name() == "copy" || b.Name() == "delete" || b.Name() == "clear")
	}
	fn := calledFunc(p.info, call)
	switch {
	case fn == nil:
		return true
	case p.gormMethod(fn):
		return !textMethods[fn.Name()] || fn.Name() == "Pluck"
	}
	switch pkgPath(fn) {
	case gormPkg:
		return fn.Name() != "Expr"
	case "fmt", "strings":
		return false
	case "slices":
		return !sliceReaders[fn.Name()]
	}
	return true
}

// sliceReaders are the functions of package slices that only read their
// arguments.
var sliceReaders = map[string]bool{"Concat": true, "Clone": true, "Contains": true, "Index": true, "Equal": true}

// isList reports a map, slice or array type.
func isList(t types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Underlying().(type) {
	case *types.Map, *types.Slice, *types.Array:
		return true
	}
	return false
}

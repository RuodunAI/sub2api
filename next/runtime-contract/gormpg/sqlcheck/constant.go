package sqlcheck

import (
	"go/ast"
	"go/token"
	"go/types"
)

// index is what constSQL follows a value through in the checked package:
// every value given to a variable or field (nil: an unknown one), the
// variables declared with a value (var, :=, range), the declarations and
// calls of its functions, which of them are used other than called, and
// the parameters whose value the calls give.
type index struct {
	funcs    map[*types.Func]*ast.FuncDecl
	values   map[*types.Var][]ast.Expr
	declared map[*types.Var]bool
	calls    map[*types.Func][]*ast.CallExpr
	escapes  map[*types.Func]bool
	params   map[*types.Var]slot
	// memo holds the verdicts on variables and results; cyclic is set when
	// one leaned on a verdict still being reached (provisional, not kept).
	memo   map[any]int8
	cyclic bool
}

// slot is parameter or result i of fn.
type slot struct {
	fn *types.Func
	i  int
}

// resultOf is the value a, b := call() gives the variable at i.
type resultOf struct {
	*ast.CallExpr
	i int
}

const (
	memoVisiting int8 = iota + 1
	memoYes
	memoNo
)

// zero stands for the zero value of a variable declared without one: "",
// or a nil list.
var zero ast.Expr = &ast.Ident{Name: "zero"}

// indexFiles records, for constSQL, every value given to a variable or
// field of the package and every declaration and call of its functions.
func (p *pass) indexFiles(files []*ast.File) {
	x := &p.index
	*x = index{funcs: map[*types.Func]*ast.FuncDecl{}, values: map[*types.Var][]ast.Expr{}, declared: map[*types.Var]bool{},
		calls: map[*types.Func][]*ast.CallExpr{}, escapes: map[*types.Func]bool{}, params: map[*types.Var]slot{}, memo: map[any]int8{}}
	called := map[*ast.Ident]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				p.indexFunc(n)
			case *ast.AssignStmt:
				if n.Tok == token.ASSIGN || n.Tok == token.DEFINE || n.Tok == token.ADD_ASSIGN {
					p.giveAll(n.Lhs, n.Rhs)
				} else {
					p.giveAll(n.Lhs, nil)
				}
			case *ast.ValueSpec:
				names := make([]ast.Expr, len(n.Names))
				for i, id := range n.Names {
					names[i] = id
				}
				if len(n.Values) == 0 {
					p.giveAll(names, []ast.Expr{zero})
				} else {
					p.giveAll(names, n.Values)
				}
			case *ast.RangeStmt:
				p.give(n.Key, nil)
				p.give(n.Value, &ast.IndexExpr{X: n.X}) // an element of what is ranged over
			case *ast.CompositeLit:
				p.indexLiteral(n)
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					p.give(n.X, nil) // address taken: written through a pointer
				}
			case *ast.CallExpr:
				var id *ast.Ident
				switch fun := ast.Unparen(n.Fun).(type) {
				case *ast.Ident:
					id = fun
				case *ast.SelectorExpr:
					id = fun.Sel
				}
				if fn, ok := p.info.Uses[id].(*types.Func); ok {
					called[id] = true
					x.calls[fn.Origin()] = append(x.calls[fn.Origin()], n)
				}
				for i, a := range n.Args {
					if t := p.info.TypeOf(a); isList(t) && p.writesArg(n, i) {
						p.give(a, nil) // handed over: the callee may write it
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

// indexFunc records a function, and the parameters whose value its calls
// give: those of an unexported function without a receiver, which only
// the package calls. Any other parameter, a receiver, a result or a
// function literal's parameter, is unknown.
func (p *pass) indexFunc(decl *ast.FuncDecl) {
	fn, ok := p.info.Defs[decl.Name].(*types.Func)
	if !ok {
		return
	}
	p.funcs[fn] = decl
	if fn.Exported() || decl.Recv != nil {
		return
	}
	i := 0
	for _, field := range decl.Type.Params.List {
		for _, name := range field.Names {
			if v, ok := p.info.Defs[name].(*types.Var); ok {
				p.params[v] = slot{fn, i}
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
}

// indexLiteral records the fields a struct literal sets.
func (p *pass) indexLiteral(n *ast.CompositeLit) {
	t := p.info.TypeOf(n)
	if ptr, isPtr := t.Underlying().(*types.Pointer); isPtr { // an element &T{} with the type elided
		t = ptr.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i, el := range n.Elts {
		if kv, keyed := el.(*ast.KeyValueExpr); keyed {
			if f, isVar := p.info.Uses[kv.Key.(*ast.Ident)].(*types.Var); isVar {
				p.values[f] = append(p.values[f], kv.Value)
			}
		} else if i < st.NumFields() {
			p.values[st.Field(i)] = append(p.values[st.Field(i)], el)
		}
	}
}

// giveAll records what an assignment or declaration of lhs to rhs gives:
// each its value, of a call each its result, and of v, ok := m[k] the
// element; anything else is unknown.
func (p *pass) giveAll(lhs, rhs []ast.Expr) {
	for i, l := range lhs {
		var val ast.Expr
		switch {
		case len(rhs) == len(lhs) || len(rhs) == 1 && rhs[0] == zero:
			val = rhs[min(i, len(rhs)-1)]
		case len(rhs) == 1:
			switch r := ast.Unparen(rhs[0]).(type) {
			case *ast.CallExpr:
				val = resultOf{r, i}
			case *ast.IndexExpr:
				if i == 0 {
					val = r
				}
			}
		}
		p.give(l, val)
	}
}

// give records val (nil: unknown) as a value of the variable or field l
// writes, or declares; writing an element makes the whole list unknown.
func (p *pass) give(l, val ast.Expr) {
	l = ast.Unparen(l)
	if ix, ok := l.(*ast.IndexExpr); ok {
		l, val = ast.Unparen(ix.X), nil
	}
	var v *types.Var
	switch x := l.(type) {
	case *ast.Ident:
		if v, _ = p.info.Defs[x].(*types.Var); v != nil {
			p.declared[v] = true
		} else {
			v, _ = p.info.Uses[x].(*types.Var)
		}
	case *ast.SelectorExpr:
		v = fieldOf(p.info, x)
	}
	if v != nil {
		p.values[v] = append(p.values[v], val)
	}
}

// constSQL reports whether e, a string, is built from constants alone: a
// constant or a value of a Policy.ConstTypes type; + of such; a variable
// or unexported field of the package that is given only such (constVar);
// an element of a list of such; a conversion of such, fmt.Sprintf,
// strings.Join, Repeat and the trims of such; a quoting helper
// (Policy.Quoting); the TableName of a generated model; a call of a
// function of the package whose every return gives such.
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
		return v != nil && p.constVar(v)
	case *ast.SelectorExpr:
		return p.constField(x)
	case *ast.IndexExpr:
		return p.constList(x.X)
	case *ast.CallExpr:
		return p.constCall(x)
	}
	return false
}

// constList reports a list of strings built from constants: a literal of
// such, a variable or field given only such lists, append and
// slices.Concat of such, a call of a function of the package whose every
// return gives such.
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
		return v != nil && isList(v.Type()) && p.constVar(v)
	case *ast.SelectorExpr:
		return p.constField(x)
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
			elems := call.Args[1:]
			if call.Ellipsis.IsValid() { // append(list, other...)
				elems = nil
			}
			return all(call.Args[:len(call.Args)-len(elems)], p.constList) && all(elems, p.constSQL)
		}
	}
	fn := calledFunc(p.info, call)
	switch {
	case fn == nil:
		return false
	case pkgPath(fn) == "slices" && fn.Name() == "Concat":
		return all(call.Args, p.constList)
	}
	return p.constResult(fn, 0)
}

// all reports whether every one of es satisfies ok.
func all(es []ast.Expr, ok func(ast.Expr) bool) bool {
	for _, e := range es {
		if !ok(e) {
			return false
		}
	}
	return true
}

// constOf is what makes a value of type t built from constants: constList
// for a list, constSQL for anything else.
func (p *pass) constOf(t types.Type) func(ast.Expr) bool {
	if isList(t) {
		return p.constList
	}
	return p.constSQL
}

// constVar reports a variable or field of the package every value given
// to which is built from constants: a list for a list, a string otherwise.
// A variable starts as the value it is declared with (var without one: the
// zero value), a field as the zero value, a parameter of an unexported
// function without a receiver as what its calls pass. Any other variable
// is unknown: one declared elsewhere, an exported package variable
// (another package may write it), any other parameter, a receiver, a
// named result, a type switch's binding.
func (p *pass) constVar(v *types.Var) bool {
	if v.Pkg() == nil || p.rel(v.Pkg().Path()) != p.pkg || v.Exported() && v.Parent() == v.Pkg().Scope() {
		return false
	}
	ok := p.constOf(v.Type())
	return p.memoized(v, func() bool {
		par, isParam := p.params[v]
		if !isParam && !p.declared[v] && !v.IsField() {
			return false
		}
		if isParam && !p.constParam(par, ok) {
			return false
		}
		for _, val := range p.values[v] {
			if r, isResult := val.(resultOf); isResult {
				fn := calledFunc(p.info, r.CallExpr)
				if fn == nil || r.Ellipsis.IsValid() || !p.constResult(fn, r.i) {
					return false
				}
			} else if val != zero && (val == nil || !ok(val)) {
				return false
			}
		}
		return true
	})
}

// constField reports a field read whose every value is built from
// constants: an unexported field of the package (constVar; decoders set
// exported ones by reflection, past what the index sees), or the SQL of a
// clause.Expr or NamedExpr, which every literal and assignment of them
// must give constant text (gorm.Expr's argument too).
func (p *pass) constField(sel *ast.SelectorExpr) bool {
	v := fieldOf(p.info, sel)
	if v == nil {
		return false
	}
	if s := p.info.Selections[sel]; v.Name() == "SQL" && (isNamed(s.Recv(), clausePkg, "Expr") || isNamed(s.Recv(), clausePkg, "NamedExpr")) {
		return true
	}
	return !v.Exported() && p.constVar(v)
}

// constParam reports parameter par, of a function that is only ever
// called, when every call passes constant text (a list: ok) there.
func (p *pass) constParam(par slot, ok func(ast.Expr) bool) bool {
	calls := p.calls[par.fn]
	if p.escapes[par.fn] || len(calls) == 0 {
		return false
	}
	sig := par.fn.Type().(*types.Signature)
	variadic := sig.Variadic() && par.i == sig.Params().Len()-1
	for _, call := range calls {
		args := call.Args
		switch {
		case variadic && !call.Ellipsis.IsValid(): // f(a, "x", "y"): each is an element
			if !all(args[min(par.i, len(args)):], p.constSQL) {
				return false
			}
		case par.i >= len(args) || !ok(args[par.i]):
			return false
		}
	}
	return true
}

// constCall reports a call whose string result is built from constants.
func (p *pass) constCall(call *ast.CallExpr) bool {
	if tv, ok := p.info.Types[call.Fun]; ok && tv.IsType() {
		return len(call.Args) == 1 && p.constSQL(call.Args[0]) // a conversion
	}
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok { // func() string { ... }()
		return len(call.Args) == 0 && p.constReturns(lit.Body, 0, 1, p.constSQL)
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
		return all(call.Args, p.constSQL)
	case path == "strings" && fn.Name() == "Join":
		return len(call.Args) == 2 && p.constList(call.Args[0]) && p.constSQL(call.Args[1])
	case path == "strings" && fn.Name() == "Repeat":
		return len(call.Args) == 2 && p.constSQL(call.Args[0]) // the count only repeats constant text
	}
	return p.constResult(fn, 0)
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

// constResult reports a function of the package each of whose returns
// gives a result i built from constants (a list for a list): what it
// returns of its parameters counts only as constParam proves them.
func (p *pass) constResult(fn *types.Func, i int) bool {
	fn = fn.Origin()
	decl := p.funcs[fn]
	res := fn.Type().(*types.Signature).Results()
	if decl == nil || decl.Body == nil || i >= res.Len() {
		return false
	}
	return p.memoized(slot{fn, i}, func() bool { return p.constReturns(decl.Body, i, res.Len(), p.constOf(res.At(i).Type())) })
}

// constReturns reports a function body each of whose returns gives n
// results, result i satisfying ok.
func (p *pass) constReturns(body *ast.BlockStmt, i, n int, ok func(ast.Expr) bool) bool {
	res := true
	ast.Inspect(body, func(node ast.Node) bool {
		switch r := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			res = res && len(r.Results) == n && ok(r.Results[i])
		}
		return res
	})
	return res
}

// memoized is eval's verdict on key, a variable or a result, kept unless
// it leaned on a verdict still being reached: a cycle adds no value of its
// own, so that one counts as constant for the while.
func (p *pass) memoized(key any, eval func() bool) bool {
	switch p.memo[key] {
	case memoVisiting:
		p.cyclic = true
		return true
	case memoYes:
		return true
	case memoNo:
		return false
	}
	p.memo[key] = memoVisiting
	outer := p.cyclic
	p.cyclic = false
	res := eval()
	switch {
	case !res:
		p.memo[key] = memoNo
	case p.cyclic:
		delete(p.memo, key)
	default:
		p.memo[key] = memoYes
	}
	p.cyclic = outer || p.cyclic
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

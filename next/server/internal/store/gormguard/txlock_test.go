package gormguard

import (
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
	"slices"
)

// ruleTxLock: a function that locks needs the transaction it is handed.
const ruleTxLock = "[tx-lock]"

// lockSQL matches SQL text that takes a lock held until the transaction
// ends: a row lock clause or a transaction-scoped advisory lock.
var lockSQL = regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+UPDATE|UPDATE|KEY\s+SHARE|SHARE)\b|_xact_lock`)

// txLock reports a function that runs a statement with a lock held until
// the transaction ends on a handle it does not check with store.RequireTx
// ([tx-lock]): on the pool such a lock ends with its own statement, and a
// read-modify-write under it silently loses updates. *gorm.DB no longer
// says "transaction" the way pgx.Tx did, so the check stands in for the
// type.
//
// A lock is a row lock (any value of type store.RowLock or clause.Locking:
// store.ForUpdate, store.Lock, a literal, a variable or field holding one),
// SQL text that locks (FOR UPDATE / NO KEY UPDATE / SHARE / KEY SHARE,
// pg_advisory_xact_lock and its kin), or a call of a lock builder of the
// package (a function that returns a *gorm.DB that locks: it only builds
// the statement and is not reported itself). A lock handed to a gorm
// method, or a lock builder's call, locks on a handle: the variables the
// statement is built on (lockSites), through method chains, store.G and
// local variables. RequireTx must be called on one of them; it may stand
// under the condition that takes the lock. Such a handle is a parameter, a
// local, a struct field or a package variable; the pool itself (store.DB's
// Gorm), on which a lock can only serve the one statement (UPDATE ...
// WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)), and the parameter of a
// function literal passed straight to store.DB.Tx or TxOptions, a
// transaction, are not checked. Nor, in a function that takes no
// *gorm.DB, is any other function literal's parameter: the function has no
// handle to hand it, so the literal's comes from what it is passed to (a
// helper of the package over store.DB.Tx), which is not followed. A lock whose
// handle is not seen (one built into a variable and run by a function of
// another package) needs a RequireTx of any handle, in a function that
// takes a *gorm.DB. What functions of other packages do is not followed:
// each checks its own.
func (c *checker) txLock(fn *ast.FuncDecl) {
	if c.test || fn.Body == nil || returnsGormDB(c.info, fn) {
		return
	}
	sites, unseen := c.lockSites(fn.Body, takesGormDB(c.info, fn))
	checked := c.requireTxRoots(fn.Body)
	ok := !unseen || !takesGormDB(c.info, fn) || len(checked) > 0
	for _, s := range sites {
		ok = ok && (len(s) == 0 || slices.ContainsFunc(s, func(v *types.Var) bool { return checked[v] }))
	}
	if !ok {
		c.report(fn.Name, "%s %s takes a lock held until the transaction ends on a *gorm.DB it does not check: call store.RequireTx on that handle first (on the pool the lock ends with its statement)", ruleTxLock, fn.Name.Name)
	}
}

// lockSites returns, for each statement of body (closures included) that
// locks on a handle it sees, the variables the handle is built on that
// RequireTx must check (none: a handle txLock does not check), and whether
// body holds a lock whose handle it does not see. handles tells whether
// body's function takes a *gorm.DB, which its function literals may be
// handed.
func (c *checker) lockSites(body ast.Node, handles bool) (sites [][]*types.Var, unseen bool) {
	sited := map[ast.Node]bool{} // the locks of the sites
	site := func(roots []*types.Var) {
		if len(roots) == 0 {
			unseen = true
			return
		}
		sites = append(sites, slices.DeleteFunc(roots, func(v *types.Var) bool {
			tx, lit := c.litParams[v]
			return c.isPool(v) || tx || lit && !handles
		}))
	}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calledFunc(c.info, call)
		switch {
		case fn == nil:
		case isGormMethod(fn):
			for _, a := range call.Args {
				if c.locks(a, sited) {
					sel, _ := ast.Unparen(call.Fun).(*ast.SelectorExpr)
					site(c.roots(sel.X, map[*types.Var]bool{}))
					c.markLocks(a, sited)
				}
			}
		case c.lockBuilder(fn):
			var roots []*types.Var
			for _, a := range call.Args {
				if isNamed(c.info.TypeOf(a), gormPkg, "DB") {
					roots = append(roots, c.roots(a, map[*types.Var]bool{})...)
				}
			}
			site(roots)
			sited[call] = true
		}
		return true
	})
	return sites, unseen || c.locks(body, sited)
}

// markLocks marks the locks of a sited.
func (c *checker) markLocks(a ast.Node, sited map[ast.Node]bool) {
	ast.Inspect(a, func(n ast.Node) bool {
		if e, ok := n.(ast.Expr); ok && c.isLock(e) {
			sited[e] = true
			return false
		}
		return true
	})
}

// fieldRoots returns the roots of what the package writes into the field
// f: f itself when it writes nothing, or something without one.
func (c *checker) fieldRoots(f *types.Var, seen map[*types.Var]bool) []*types.Var {
	if seen[f] {
		return nil
	}
	seen[f] = true
	if c.isPool(f) {
		return []*types.Var{f}
	}
	vals := c.fieldValues[f]
	var out []*types.Var
	for _, v := range vals {
		roots := []*types.Var(nil)
		if v != nil {
			roots = c.roots(v, seen)
		}
		if len(roots) == 0 {
			return []*types.Var{f}
		}
		out = append(out, roots...)
	}
	if len(out) == 0 {
		return []*types.Var{f}
	}
	return out
}

// isPool reports the Gorm field of store.DB: the pool's handle.
func (c *checker) isPool(v *types.Var) bool {
	return v.IsField() && v.Name() == "Gorm" && v.Pkg() != nil && v.Pkg().Path() == c.storePkg
}

// requireTxRoots returns the variables the handles store.RequireTx is
// called on in body are built on.
func (c *checker) requireTxRoots(body ast.Node) map[*types.Var]bool {
	out := map[*types.Var]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if fn := calledFunc(c.info, call); fn != nil && c.isStoreFunc(fn, "RequireTx") {
				for _, v := range c.roots(call.Args[0], map[*types.Var]bool{}) {
					out[v] = true
				}
			}
		}
		return true
	})
	return out
}

// roots returns the variables the *gorm.DB e is built on: e itself when it
// is a parameter, field or package variable; the values of a local
// (itself when one is unknown); through parentheses, gorm methods that
// return a *gorm.DB, store.G and the functions of the package that take
// and return one. Nil when e is built on none of those.
func (c *checker) roots(e ast.Expr, seen map[*types.Var]bool) []*types.Var {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		v, _ := c.info.Uses[x].(*types.Var)
		if v == nil || seen[v] {
			return nil
		}
		seen[v] = true
		as := c.assigns[v]
		out := []*types.Var{}
		if c.localVar(x) == nil || c.params[v] || len(as) == 0 {
			out = append(out, v)
		}
		for _, a := range as {
			if a.rhs == nil {
				out = append(out, v)
				continue
			}
			out = append(out, c.roots(a.rhs, seen)...)
		}
		return out
	case *ast.SelectorExpr:
		if s := c.info.Selections[x]; s != nil && s.Kind() == types.FieldVal {
			return c.fieldRoots(s.Obj().(*types.Var), seen)
		}
		if v, ok := c.info.Uses[x.Sel].(*types.Var); ok { // a package variable of another package
			return []*types.Var{v}
		}
	case *ast.CallExpr:
		fn := calledFunc(c.info, x)
		switch {
		case fn == nil:
		case isGormMethod(fn):
			if sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
				return c.roots(sel.X, seen)
			}
		case c.isStoreFunc(fn, "G") && len(x.Args) == 2:
			return c.roots(x.Args[1], seen)
		case c.funcs[fn.Origin()] != nil && returnsGormDBSig(fn):
			var out []*types.Var
			for _, a := range x.Args {
				if isNamed(c.info.TypeOf(a), gormPkg, "DB") {
					out = append(out, c.roots(a, seen)...)
				}
			}
			return out
		}
	}
	return nil
}

// locks reports whether n holds a lock (txLock) that sited does not hold,
// closures included.
func (c *checker) locks(n ast.Node, sited map[ast.Node]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sited[n] {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if fn := calledFunc(c.info, call); fn != nil && c.lockBuilder(fn) {
				found = true
			}
		}
		if e, ok := n.(ast.Expr); ok && c.isLock(e) {
			found = true
		}
		return !found
	})
	return found
}

// isLock reports a row lock value or SQL text that locks.
func (c *checker) isLock(e ast.Expr) bool {
	tv, ok := c.info.Types[e]
	if !ok || tv.IsType() {
		return false
	}
	if isNamed(tv.Type, c.storePkg, "RowLock") || isNamed(tv.Type, clausePkg, "Locking") {
		return true
	}
	return tv.Value != nil && tv.Value.Kind() == constant.String && lockSQL.MatchString(constant.StringVal(tv.Value))
}

// lockBuilder reports whether fn is a function of the package that returns
// one *gorm.DB and locks (memoized).
func (c *checker) lockBuilder(fn *types.Func) bool {
	fn = fn.Origin()
	if b, ok := c.lockFns[fn]; ok {
		return b
	}
	c.lockFns[fn] = false // while it is being looked at: recursion adds no lock
	decl := c.funcs[fn]
	if decl == nil || decl.Body == nil || !returnsGormDB(c.info, decl) {
		return false
	}
	c.lockFns[fn] = c.locks(decl.Body, nil)
	return c.lockFns[fn]
}

// isStoreFunc reports the package-level function name of package store.
func (c *checker) isStoreFunc(fn *types.Func, name string) bool {
	return fn.Name() == name && fn.Pkg() != nil && fn.Pkg().Path() == c.storePkg && fn.Type().(*types.Signature).Recv() == nil
}

// takesGormDB reports a function with a *gorm.DB parameter.
func takesGormDB(info *types.Info, fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		if isNamed(info.TypeOf(field.Type), gormPkg, "DB") {
			return true
		}
	}
	return false
}

// returnsGormDB reports a function whose results include a *gorm.DB: it
// builds a statement that its caller runs.
func returnsGormDB(info *types.Info, fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if isNamed(info.TypeOf(field.Type), gormPkg, "DB") {
			return true
		}
	}
	return false
}

// returnsGormDBSig reports a function whose results include a *gorm.DB.
func returnsGormDBSig(fn *types.Func) bool {
	res := fn.Type().(*types.Signature).Results()
	for i := range res.Len() {
		if isNamed(res.At(i).Type(), gormPkg, "DB") {
			return true
		}
	}
	return false
}

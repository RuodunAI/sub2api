package gormguard

import (
	"go/ast"
	"go/constant"
	"go/types"
	"regexp"
)

// ruleTxLock: a function that locks needs the transaction it is handed.
const ruleTxLock = "[tx-lock]"

// lockSQL matches SQL text that takes a lock held until the transaction
// ends: a row lock clause or a transaction-scoped advisory lock.
var lockSQL = regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+UPDATE|UPDATE|KEY\s+SHARE|SHARE)\b|_xact_lock`)

// txLock reports a function that takes a *gorm.DB, runs a statement with a
// lock held until the transaction ends, and does not check with
// store.RequireTx that the handle is a transaction ([tx-lock]): on the pool
// such a lock ends with its own statement, and a read-modify-write under it
// silently loses updates. *gorm.DB no longer says "transaction" the way
// pgx.Tx did, so the check stands in for the type.
//
// A function locks when its body (closures included) names a row lock
// (store.ForUpdate, store.Lock, a clause.Locking literal), holds SQL text
// that locks (FOR UPDATE / NO KEY UPDATE / SHARE / KEY SHARE,
// pg_advisory_xact_lock and its kin), or calls a lock builder of its
// package. A lock builder returns a *gorm.DB that locks: it only builds the
// statement, so it is not reported itself, and the function that runs it
// checks. What functions of other packages do is not followed: each checks
// its own.
func (c *checker) txLock(fn *ast.FuncDecl) {
	if c.test || fn.Body == nil || !takesGormDB(c.info, fn) || returnsGormDB(c.info, fn) {
		return
	}
	if c.locks(fn.Body) && !c.callsRequireTx(fn.Body) {
		c.report(fn.Name, "%s %s takes a lock held until the transaction ends on a *gorm.DB it does not check: call store.RequireTx first (on the pool the lock ends with its statement)", ruleTxLock, fn.Name.Name)
	}
}

// locks reports whether body takes a transaction-scoped lock (txLock).
func (c *checker) locks(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			found = found || isNamed(c.info.TypeOf(n), clausePkg, "Locking")
		case *ast.CallExpr:
			fn := calledFunc(c.info, n)
			found = found || fn != nil && (c.isStoreFunc(fn, "ForUpdate") || c.isStoreFunc(fn, "Lock") || c.lockBuilder(fn))
		case ast.Expr:
			if tv, ok := c.info.Types[n]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
				found = found || lockSQL.MatchString(constant.StringVal(tv.Value))
			}
		}
		return !found
	})
	return found
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
	c.lockFns[fn] = c.locks(decl.Body)
	return c.lockFns[fn]
}

// callsRequireTx reports whether body calls store.RequireTx.
func (c *checker) callsRequireTx(body ast.Node) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if fn := calledFunc(c.info, call); fn != nil && c.isStoreFunc(fn, "RequireTx") {
				found = true
			}
		}
		return !found
	})
	return found
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

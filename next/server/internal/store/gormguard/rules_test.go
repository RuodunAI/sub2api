package gormguard

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg/sqlcheck"
)

// Rule tags, at the start of every finding.
const (
	ruleTx       = "[gorm-tx]"
	ruleOpen     = "[gorm-open]"
	ruleInstance = "[gorm-instance]"
	ruleSlice    = "[gorm-slice]"
	ruleUnrun    = "[gorm-raw-unrun]"
	rulePgx      = "[pgx-import]"
	ruleSQL      = "[sql-handle]"
	ruleTestutil = "[testutil-import]"
	ruleTxPool   = "[tx-pool]"
	ruleCasbin   = "[casbin-api]"
	ruleCore     = "[core-store]"
)

// bannedMethods are the gorm (*DB) methods that would let GORM own a
// transaction, a connection or the schema: start one (Transaction, Begin),
// end the transaction a handle runs in (Commit, Rollback) or part of it
// (SavePoint, RollbackTo), take a connection of the pool (Connection), or
// change the schema (AutoMigrate, Migrator).
var bannedMethods = map[string]bool{
	"Transaction": true, "Begin": true, "Commit": true, "Rollback": true, "SavePoint": true, "RollbackTo": true,
	"Connection": true, "AutoMigrate": true, "Migrator": true,
}

// storeTxMethods are the store.DB methods whose function argument runs on
// one transaction or connection of the pool; gormTxMethods are gorm's own
// (in the owners).
var (
	storeTxMethods = map[string]bool{"Tx": true, "TxOptions": true, "WithConn": true}
	gormTxMethods  = map[string]bool{"Transaction": true, "Connection": true}
)

// instanceMethods are the gorm (*DB) methods that reach or change the
// shared instance behind every handle: its *sql.DB, callbacks and plugins.
// Debug is a session that logs every statement with its values (Info
// level), past the instance's logger.Discard.
var instanceMethods = map[string]bool{"DB": true, "Callback": true, "Use": true, "Debug": true}

// sessionOverrides are the gorm.Session fields that override, for the
// statements of a session, what the instance was configured with on
// purpose: PrepareStmt runs them as *sql.Stmt on the pool, past sqlpool's
// result reading (the RowsAffected wait it prevents comes back);
// AllowGlobalUpdate lets an UPDATE or DELETE without WHERE through; Logger
// logs the SQL with its values; SkipDefaultTransaction=false wraps writes
// in a transaction of their own, outside store.DB.Tx; DryRun builds
// statements that never run.
var sessionOverrides = map[string]bool{
	"PrepareStmt": true, "AllowGlobalUpdate": true, "Logger": true, "SkipDefaultTransaction": true, "DryRun": true,
}

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

type rules struct {
	module    string
	owners    map[string]string // packages that own the instance, relative to the module
	sqlOwners map[string]string // packages that may use store.DB.SQL, relative to the module
	pgxFiles  map[string]string // the files that may import pgx, relative to the module
	// casbinOwners may import gorm-adapter and call Casbin's Enforce
	// (casbin_test.go).
	casbinOwners map[string]string
	// sql runs [raw-sql] and [sql-const] with the core's policy
	// (rawsql_test.go); nil leaves both rules off.
	sql *sqlcheck.Checker
}

// check returns the findings of one type-checked package as
// "file:line:col: [rule] message", but for [pgx-import] (imports). The
// files must be parsed with comments, and info must hold Defs as well as
// Types, Uses and Selections.
func (r rules) check(fset *token.FileSet, pkgPath string, files []*ast.File, info *types.Info) []string {
	rel := strings.TrimPrefix(pkgPath, r.module+"/")
	_, owner := r.owners[rel]
	_, sqlOwner := r.sqlOwners[rel]
	c := &checker{fset: fset, info: info, owner: owner, storePkg: r.module + "/" + storePkg,
		casbinOwner: listedWithin(r.casbinOwners, rel)}
	c.index(files)
	for _, f := range files {
		name := filepath.Base(fset.Position(f.Package).Filename)
		test := strings.HasSuffix(name, "_test.go")
		c.sqlOwner = sqlOwner || test
		c.test = test
		ast.Inspect(f, c.visit)
	}
	if r.sql != nil {
		c.out = append(c.out, r.sql.Check(fset, pkgPath, files, info)...)
	}
	return c.out
}

// imports returns the [pgx-import] findings of f, a file of the package
// rel (relative to the module): pgx is imported by the files of pgxFiles
// only, test files included.
func (r rules) imports(fset *token.FileSet, rel string, f *ast.File) []string {
	name := filepath.Base(fset.Position(f.Package).Filename)
	_, pgxFile := r.pgxFiles[rel+"/"+name]
	// [testutil-import]: internal/testutil owns the instance as test
	// infrastructure, so only test files (and its own) may import it.
	production := !strings.HasSuffix(name, "_test.go") && !within(rel, testutilPkg)
	var out []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		switch {
		case err != nil:
		case !pgxFile && within(path, pgxModule):
			out = append(out, fmt.Sprintf("%s: %s import of %s: statements go through GORM and PostgreSQL errors through store (pgerror.go is the one file that reads pgconn's errors)",
				fset.Position(imp.Pos()), rulePgx, path))
		case production && within(path, r.module+"/"+testutilPkg):
			out = append(out, fmt.Sprintf("%s: %s import of %s outside the test files: it owns the GORM instance and the pool as test infrastructure only",
				fset.Position(imp.Pos()), ruleTestutil, path))
		case production && within(rel, corePkg) && within(path, r.module+"/"+storePkg):
			out = append(out, fmt.Sprintf("%s: %s import of %s in core: core is the contract the modules share, store implements it; the GORM glue lives in store or the caller",
				fset.Position(imp.Pos()), ruleCore, path))
		case within(path, adapterModule) && !listedWithin(r.casbinOwners, rel):
			out = append(out, fmt.Sprintf("%s: %s import of %s outside %s: Casbin reads the policy there, through the adapter's read-only load alone",
				fset.Position(imp.Pos()), ruleCasbin, path, casbinxPkg))
		}
	}
	return out
}

// The packages that the import rules name, relative to the module.
const (
	testutilPkg = "internal/testutil" // test infrastructure
	corePkg     = "internal/core"     // the modules' contract ([core-store])
	storePkg    = "internal/store"
)

// within reports whether path is pkg or a package under it.
func within(path, pkg string) bool { return path == pkg || strings.HasPrefix(path, pkg+"/") }

type checker struct {
	fset            *token.FileSet
	info            *types.Info
	owner, sqlOwner bool
	storePkg        string // import path of package store
	out             []string

	// [casbin-api]: whether the package owns Casbin.
	casbinOwner bool
	// test is whether the file being checked is a test file ([tx-lock]
	// leaves them alone).
	test bool

	// What [gorm-raw-unrun] follows a Raw statement through: the functions
	// of the package, the assignments and uses of its local variables, and
	// whether a function returns an unrun Raw statement (memoized).
	funcs   map[*types.Func]*ast.FuncDecl
	assigns map[*types.Var][]assignment
	uses    map[*types.Var][]use
	rawFns  map[*types.Func]bool
	// [tx-lock]: whether a function of the package is a lock builder
	// (memoized), and the parameters of its functions and of its function
	// literals.
	lockFns           map[*types.Func]bool
	params, litParams map[*types.Var]bool
	fieldValues       map[*types.Var][]ast.Expr // what the package writes into a field (nil: unknown)

	// [casbin-api]: the bodies of the package's functions and function
	// literals, and the gorm-adapter constructor selectors whose call
	// passed adapterTurnedOff.
	bodies        []*ast.BlockStmt
	adapterVetted map[*ast.SelectorExpr]bool
	// casbinCtorVetted are the enforcer constructors casbinCall saw called
	// as casbinx may call them (casbinCtor).
	casbinCtorVetted map[*ast.SelectorExpr]bool
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
	c.lockFns = map[*types.Func]bool{}
	c.params, c.litParams = map[*types.Var]bool{}, map[*types.Var]bool{}
	c.fieldValues = map[*types.Var][]ast.Expr{}
	params := func(fields *ast.FieldList, lit bool) {
		for _, f := range fields.List {
			for _, name := range f.Names {
				if v, ok := c.info.Defs[name].(*types.Var); ok {
					c.params[v], c.litParams[v] = true, lit
				}
			}
		}
	}
	c.adapterVetted = map[*ast.SelectorExpr]bool{}
	c.casbinCtorVetted = map[*ast.SelectorExpr]bool{}
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
				if n.Body != nil {
					c.bodies = append(c.bodies, n.Body)
				}
				params(n.Type.Params, false)
			case *ast.FuncLit:
				c.bodies = append(c.bodies, n.Body)
				params(n.Type.Params, true)
			case *ast.AssignStmt:
				assign(n.End(), n.Lhs, n.Rhs)
				for i, l := range n.Lhs {
					if sel, ok := ast.Unparen(l).(*ast.SelectorExpr); ok {
						if s := c.info.Selections[sel]; s != nil && s.Kind() == types.FieldVal {
							f := s.Obj().(*types.Var)
							if len(n.Lhs) == len(n.Rhs) {
								c.fieldValues[f] = append(c.fieldValues[f], n.Rhs[i])
							} else {
								c.fieldValues[f] = append(c.fieldValues[f], nil)
							}
						}
					}
				}
			case *ast.CompositeLit:
				c.literalFields(n)
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

// literalFields records the fields a struct literal sets (fieldValues).
func (c *checker) literalFields(n *ast.CompositeLit) {
	t := c.info.TypeOf(n)
	if p, ok := t.Underlying().(*types.Pointer); ok {
		t = p.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i, el := range n.Elts {
		if kv, keyed := el.(*ast.KeyValueExpr); keyed {
			if f, ok := c.info.Uses[kv.Key.(*ast.Ident)].(*types.Var); ok {
				c.fieldValues[f] = append(c.fieldValues[f], kv.Value)
			}
		} else if i < st.NumFields() {
			c.fieldValues[st.Field(i)] = append(c.fieldValues[st.Field(i)], el)
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
		if sel := c.info.Selections[n]; sel != nil && sel.Kind() == types.FieldVal {
			c.sqlHandle(n.Sel, sel.Obj())
			c.connPool(n, sel.Obj())
		}
	case *ast.KeyValueExpr:
		if key, ok := n.Key.(*ast.Ident); ok {
			c.sqlHandle(key, c.info.Uses[key])
		}
	case *ast.CallExpr:
		c.call(n)
	case *ast.AssignStmt:
		c.assign(n)
	case *ast.CompositeLit:
		c.literal(n)
	case *ast.FuncDecl:
		c.txLock(n)
	}
	return true
}

// selector checks the functions a selector names: selectors, not calls, so
// that a method value (f := db.Begin) is caught as well.
func (c *checker) selector(n *ast.SelectorExpr) {
	fn := funcOf(c.info, n)
	switch {
	case fn == nil:
	case isGormMethod(fn) && bannedMethods[fn.Name()] && !c.owner:
		c.report(n, "%s gorm (*DB).%s outside package store: transactions start and end in store.DB.Tx or TxOptions, connections come from WithConn, and the schema comes from the SQL migrations", ruleTx, fn.Name())
	case isGormMethod(fn) && instanceMethods[fn.Name()] && !c.owner:
		c.report(n, "%s gorm (*DB).%s outside package store: every handle shares store.G's instance, with its pool, callbacks and silent logger", ruleInstance, fn.Name())
	case isGormFunc(fn, "Open") && !c.owner:
		c.report(n, "%s gorm.Open outside package store: take a handle from store.G", ruleOpen)
	case fn.Name() == "SQLTx" && fn.Pkg() != nil && fn.Pkg().Path() == c.storePkg && !c.sqlOwner:
		c.report(n, "%s store.SQLTx outside package store, River (internal/task) and the tests: run the statement on the transaction's GORM handle", ruleSQL)
	default:
		c.casbinSelector(n, fn)
	}
}

// connPool reports a ConnPool of gorm (a Statement's, or the instance
// configuration's) outside the owners, read or written ([gorm-instance]):
// it is the pool, transaction or connection under a handle, and what runs
// on it bypasses GORM and the transaction rules.
func (c *checker) connPool(n *ast.SelectorExpr, obj types.Object) {
	if c.owner || obj.Name() != "ConnPool" || obj.Pkg() == nil || obj.Pkg().Path() != gormPkg {
		return
	}
	c.report(n, "%s gorm ConnPool outside package store: run statements through the handle (store.G), not the connection under it", ruleInstance)
}

// txCallbacks reports, in the function literals passed to store.DB's Tx,
// TxOptions and WithConn and to gorm's Transaction and Connection, every
// expression that is a store.DB ([tx-pool]): the pool, which the callback
// must not use while it holds a transaction or connection of it. A
// statement there takes a second connection while holding one (a pool
// that runs out waits forever), and does not see the transaction's writes
// or locks. Each reference is reported once, at the store.DB itself (s.db
// in s.db.Gorm); what other functions do with the pool is not followed.
func (c *checker) txCallbacks(fn *types.Func, n *ast.CallExpr) {
	if !(isMethodOf(fn, c.storePkg, "DB") && storeTxMethods[fn.Name()]) && !(isGormMethod(fn) && gormTxMethods[fn.Name()]) {
		return
	}
	for _, arg := range n.Args {
		lit, ok := ast.Unparen(arg).(*ast.FuncLit)
		if !ok {
			continue
		}
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			e, ok := m.(ast.Expr)
			if !ok {
				return true
			}
			// Types holds expressions only: no declared name, selector Sel
			// or struct literal key, so each reference is seen once.
			tv, ok := c.info.Types[e]
			if !ok || tv.IsType() || !isNamed(tv.Type, c.storePkg, "DB") {
				return true
			}
			c.report(e, "%s the pool (store.DB) inside the %s callback: run the statement on the callback's handle", ruleTxPool, fn.Name())
			return false
		})
	}
}

// sqlHandle reports a use of the SQL field of store.DB (a selector, or a
// key of a store.DB literal) outside the packages that may use it and the
// test files: production statements go through Gorm.
func (c *checker) sqlHandle(n ast.Node, obj types.Object) {
	if c.sqlOwner || !c.isSQLField(obj) {
		return
	}
	c.report(n, "%s store.DB.SQL outside package store, River (internal/task) and the tests: run the statement on store.DB.Gorm", ruleSQL)
}

// isSQLField reports whether obj is the SQL field of store.DB.
func (c *checker) isSQLField(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() || v.Name() != "SQL" || v.Pkg() == nil || v.Pkg().Path() != c.storePkg {
		return false
	}
	tn, _ := v.Pkg().Scope().Lookup("DB").(*types.TypeName)
	if tn == nil {
		return false
	}
	st, _ := tn.Type().Underlying().(*types.Struct)
	for i := 0; st != nil && i < st.NumFields(); i++ {
		if st.Field(i) == v {
			return true
		}
	}
	return false
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

func (c *checker) call(n *ast.CallExpr) {
	sel, ok := ast.Unparen(n.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	fn := funcOf(c.info, sel)
	if fn == nil {
		return
	}
	c.txCallbacks(fn, n)
	c.casbinCall(fn, n)
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

// literal checks the Vars of a clause.Expr or clause.NamedExpr literal,
// and the fields of a gorm.Session one (session).
func (c *checker) literal(n *ast.CompositeLit) {
	t := c.info.TypeOf(n)
	if isNamed(t, gormPkg, "Session") {
		c.session(n)
		return
	}
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

// session reports a gorm.Session literal that sets one of
// sessionOverrides outside the owners ([gorm-instance]): it reconfigures
// the shared instance for the statements of that session.
func (c *checker) session(n *ast.CompositeLit) {
	if c.owner {
		return
	}
	for _, e := range n.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			// Positional: the literal sets every field.
			c.report(n, "%s positional gorm.Session literal outside package store: name the fields, and set none of %s", ruleInstance, overrideNames())
			return
		}
		if key, ok := kv.Key.(*ast.Ident); ok && sessionOverrides[key.Name] {
			c.report(kv, "%s gorm.Session{%s} outside package store: it overrides the instance's configuration for the session's statements", ruleInstance, key.Name)
		}
	}
}

// overrideNames lists sessionOverrides, sorted.
func overrideNames() string {
	names := make([]string, 0, len(sessionOverrides))
	for name := range sessionOverrides {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
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
		v := assignedField(c.info, lhs)
		switch {
		case v == nil:
		case instanceField(v):
			c.report(lhs, "%s assignment to gorm %s outside package store: every handle shares store.G's instance", ruleInstance, v.Name())
		case sessionField(v):
			c.report(lhs, "%s assignment to gorm.Session's %s outside package store: it overrides the instance's configuration for the session's statements", ruleInstance, v.Name())
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
// not) or gorm.DB's Config itself. A ConnPool is connPool's, on any use.
func instanceField(v *types.Var) bool {
	if v.Pkg() == nil || v.Pkg().Path() != gormPkg {
		return false
	}
	switch v.Name() {
	case "Config":
		return true
	case "ConnPool":
		return false
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

// sessionField reports a field of gorm.Session among sessionOverrides.
func sessionField(v *types.Var) bool {
	if v.Pkg() == nil || v.Pkg().Path() != gormPkg || !sessionOverrides[v.Name()] {
		return false
	}
	tn, _ := v.Pkg().Scope().Lookup("Session").(*types.TypeName)
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

package gormguard

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// [raw-sql] and [sql-const]: statements are built by GORM, and SQL text is
// code, never data (CONTRACTS §100).
const (
	ruleRawSQL   = "[raw-sql]"
	ruleSQLConst = "[sql-const]"
)

// rawSQLAllowed lists the declarations, "package.Decl" relative to the
// module (a function, Type.Method, or the first name of a package-level
// var or const), that may call gorm's Raw or Exec or store.NewNumbered,
// with the reason. Everything else builds its statement with GORM's
// builder; an entry with no such call left fails the repository test.
var rawSQLAllowed = map[string]string{
	// The ledger: data-modifying CTEs that move money in one statement.
	"internal/billing.prechargeStmt": "ledger CTE: precharge (hold the amount, write the ledger row and the request precharge in one statement)",
	"internal/billing.applyStmt":     "ledger CTE: apply a charge or credit to user_balances and balance_ledger in one statement",
	"internal/billing.releaseStmt":   "ledger CTE: release a request precharge and refund it in one statement",
	"internal/billing.settleStmt":    "ledger CTE: settle (release the precharge, apply the charge) in one statement",
	"internal/billing.writeRules":    "price rules: history, lock, delete and both partial-index upserts of any number of changes in one data-modifying CTE and one round trip (lock order = change order); GORM needs five statements",
	// store: the runners of those statements and the helpers for what is
	// no data statement.
	"internal/store.Numbered.Raw":                  "runs a NewNumbered statement (the ledger CTEs); the text is checked where NewNumbered is called",
	"internal/store.Numbered.Exec":                 "runs a NewNumbered statement (the ledger CTEs); the text is checked where NewNumbered is called",
	"internal/store.ExistsQuery":                   "thin combinator: SELECT EXISTS (sub) around a builder query",
	"internal/store.InsertSelect":                  "thin combinator: INSERT INTO t (cols) <builder query> [ON CONFLICT], which GORM's Create cannot express",
	"internal/store.InsertSelectReturning":         "thin combinator: InsertSelect with RETURNING, scanned into the caller's rows",
	"internal/store.NextIDs":                       "nextval of a table's id sequence, for a batch insert that needs its ids first",
	"internal/store.xactLock":                      "pg_advisory_xact_lock: a transaction-scoped advisory lock",
	"internal/store.TryXactLock":                   "pg_try_advisory_xact_lock: XactLock without the wait",
	"internal/store.XactLockID":                    "pg_advisory_xact_lock of a lock named by its id",
	"internal/store.DDL":                           "DDL and SET take no parameters: a constant format with quoted identifiers and literals (the format is checked where DDL is called)",
	"internal/store.CurrentSnapshot":               "pg_current_snapshot(): server state, no table",
	"internal/store.CurrentDatabase":               "current_database(): server state, no table",
	"internal/store.RelationExists":                "to_regclass: a catalog lookup",
	"internal/plugin/dbschema.Tracker.recordQuery": "call of the void SECURITY DEFINER function plugin_migration_record as the plugin role",
	"internal/store.XactLockHashtext":              "pg_advisory_xact_lock(hashtext(key)): the 32-bit lock ids existing deployments already hold",
	"internal/store.SetConfig":                     "set_config of a transaction-local custom setting read by triggers",
	"internal/store.ClockTimestamp":                "clock_timestamp(): the database clock, outside any table",
	"internal/store.Migrate":                       "the migrations runner: session advisory lock and unlock",
	"internal/store.migrateFile":                   "the migrations runner: SET LOCAL search_path and the migration files' DDL",
	"internal/store.CoreTracker.Ensure":            "the migrations runner: DDL of schema_migrations",
	"internal/store/modelgen.FromMigrations":       "dev tool: CREATE / DROP DATABASE of a scratch database",
	"internal/store/modelgen.readTables":           "dev tool: reads the system catalog (pg_catalog)",
}

// sqlConstAllowed lists the declarations whose SQL text may be built from
// what [sql-const] cannot prove constant, with the reason.
var sqlConstAllowed = map[string]string{
	"internal/store.Numbered.Raw":            "the text NewNumbered rewrote; NewNumbered's argument is checked",
	"internal/store.Numbered.Exec":           "the text NewNumbered rewrote; NewNumbered's argument is checked",
	"internal/store/modelgen.FromMigrations": "dev tool: the scratch database's name, generated and quoted",
	"internal/usage.statRows":                "the measure columns are joined from the constant statMeasures table by measureList, whose callback [sql-const] cannot follow",
	"internal/usage.rollupHours":             "the measure columns are joined from the constant statMeasures table by measureList, whose callback [sql-const] cannot follow",
	"internal/store.DDL":                     "the format is checked where DDL is called; its arguments are quoted (Ident, Literal)",
	"internal/lookup.Service.lookup":         "columns and conditions of the fixed entities table (entities.go), chosen by kind: a whitelist whose exported fields [sql-const] does not follow",
	"internal/lookup.Entity.search":          "columns and conditions of the fixed entities table (entities.go), chosen by kind: a whitelist whose exported fields [sql-const] does not follow",
	"internal/lookup.Entity.query":           "columns and conditions of the fixed entities table (entities.go), chosen by kind: a whitelist whose exported fields [sql-const] does not follow",
	"internal/store.Conds.And":               "the text is checked where Conds.And is called",
	"internal/store.UpdateFrom":              "the text is checked where UpdateFrom is called",
}

// sqlQuoting lists the functions, "package.Func" relative to the module,
// whose result is a quoted identifier or literal: [sql-const] accepts it
// wherever constant text is required, whatever their argument.
var sqlQuoting = map[string]string{
	"internal/store.QuoteIdent":             "a quoted identifier",
	"internal/plugin/dbschema.quoteLiteral": "a quoted string literal",
}

// sqlPending lists the packages other teams are converting in parallel:
// their findings of [raw-sql] and [sql-const] are logged, not failed.
//
// ---- PENDING OTHER TEAMS: the lead removes this block after merging ----
var sqlPending = map[string]string{
	"internal/authz": "authz rewrite (Casbin)",
}

// ---- end of PENDING OTHER TEAMS ----

// sqlTextMethods are the gorm (*DB) methods whose first argument, when it
// is a string, is SQL text ([sql-const]).
var sqlTextMethods = map[string]bool{
	"Raw": true, "Exec": true, "Where": true, "Not": true, "Or": true, "Having": true, "Select": true,
	"Order": true, "Group": true, "Joins": true, "Table": true, "Distinct": true, "Pluck": true,
}

// sqlPolicy holds the allowlists of the two rules, the packages left to
// other teams, and what the check found.
type sqlPolicy struct {
	raw, constant, quoting, pending map[string]string
	used                            map[string]bool // "rule key" of the entries that allowed a finding
	pendingOut                      []string        // the findings in pending packages
}

func newSQLPolicy() *sqlPolicy {
	return &sqlPolicy{raw: rawSQLAllowed, constant: sqlConstAllowed, quoting: sqlQuoting, pending: sqlPending, used: map[string]bool{}}
}

// unused returns the allowlist entries that allowed nothing.
func (p *sqlPolicy) unused() []string {
	var out []string
	for rule, list := range map[string]map[string]string{ruleRawSQL: p.raw, ruleSQLConst: p.constant} {
		for key := range list {
			if !p.used[rule+" "+key] {
				out = append(out, rule+" "+key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sqlIndex is what [sql-const] follows a string through: the values given
// to each variable (nil: unknown), the calls of each function of the
// package, and whether a function is used other than called.
type sqlIndex struct {
	values  map[*types.Var][]ast.Expr
	ranges  map[*types.Var][]ast.Expr // ranged over: the value is an element of the expression
	calls   map[*types.Func][]*ast.CallExpr
	escapes map[*types.Func]bool
	// memo and the parameters bound while a callee's results are looked at.
	varMemo  map[*types.Var]int8
	funcMemo map[string]int8
	bound    map[*types.Var]bool
}

const (
	memoVisiting int8 = iota + 1
	memoYes
	memoNo
)

// sqlFiles checks the non-test files of the package for both rules.
func (c *checker) sqlFiles(files []*ast.File) {
	if c.sql == nil {
		return
	}
	c.sqlIndexFiles(files)
	for _, f := range files {
		if strings.HasSuffix(c.fset.Position(f.Package).Filename, "_test.go") {
			continue
		}
		combinators := map[ast.Node]bool{} // the Raw and Exec of a combinator, which [raw-sql] passes
		for _, decl := range f.Decls {
			key := c.rel + "." + declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if !combinators[n] {
						c.rawSQL(n, key)
					}
				case *ast.CallExpr:
					if c.isCombinator(n) {
						combinators[n.Fun] = true
					}
					c.constCallSQL(n, key)
				case *ast.CompositeLit:
					if t := c.info.TypeOf(n); isNamed(t, clausePkg, "Expr") || isNamed(t, clausePkg, "NamedExpr") {
						if e := fieldValue(n, "SQL", 0); e != nil {
							c.requireConst(e, key, t.String()+".SQL")
						}
					}
				}
				return true
			})
		}
	}
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

// sqlReport reports a finding of rule at n unless list allows key; in a
// pending package it is kept apart.
func (c *checker) sqlReport(n ast.Node, rule, key string, list map[string]string, format string, args ...any) {
	if _, ok := list[key]; ok {
		c.sql.used[rule+" "+key] = true
		return
	}
	if listedWithin(c.sql.pending, c.rel) {
		before := len(c.out)
		c.report(n, format, args...)
		c.sql.pendingOut = append(c.sql.pendingOut, c.out[before:]...)
		c.out = c.out[:before]
		return
	}
	c.report(n, format, args...)
}

// rawSQL reports gorm's Raw and Exec and store.NewNumbered, called or as
// values, outside rawSQLAllowed ([raw-sql]): a statement written as text
// in code. The query builder writes it instead (CONTRACTS §100).
func (c *checker) rawSQL(n *ast.SelectorExpr, key string) {
	fn := funcOf(c.info, n)
	if fn == nil || !(isGormMethod(fn) && (fn.Name() == "Raw" || fn.Name() == "Exec")) && !c.isStoreFunc(fn, "NewNumbered") {
		return
	}
	c.sqlReport(n, ruleRawSQL, key, c.sql.raw, "%s %s in %s: a statement written as SQL text; build it with GORM's query builder (models, Where, Updates, clause.OnConflict ...), or list the site in rawSQLAllowed with the reason GORM cannot express it", ruleRawSQL, fn.Name(), key)
}

// combinatorText is the text of a set operation over builder queries,
// (q1) UNION ALL (q2) ... [ORDER BY 1]: it holds no statement of its own,
// so [raw-sql] passes a Raw or Exec of it (the gateway's sqlguard alike).
var combinatorText = regexp.MustCompile(`(?i)^\s*\(\?\)(\s+(UNION|INTERSECT|EXCEPT)(\s+ALL)?\s+\(\?\))+(\s+ORDER\s+BY\s+\d+(\s*,\s*\d+)*)?\s*$`)

// isCombinator reports a Raw or Exec of gorm whose text is a constant
// combinatorText.
func (c *checker) isCombinator(call *ast.CallExpr) bool {
	fn := calledFunc(c.info, call)
	if fn == nil || !isGormMethod(fn) || fn.Name() != "Raw" && fn.Name() != "Exec" || len(call.Args) == 0 {
		return false
	}
	tv, ok := c.info.Types[call.Args[0]]
	return ok && tv.Value != nil && tv.Value.Kind() == constant.String && combinatorText.MatchString(constant.StringVal(tv.Value))
}

// constCallSQL checks the SQL text argument of a call ([sql-const]).
func (c *checker) constCallSQL(n *ast.CallExpr, key string) {
	fn := calledFunc(c.info, n)
	if fn == nil || len(n.Args) == 0 {
		return
	}
	switch {
	case isGormMethod(fn) && sqlTextMethods[fn.Name()],
		isGormFunc(fn, "Expr"),
		fn.Name() == "And" && isMethodOf(fn, c.storePkg, "Conds"),
		c.isStoreFunc(fn, "NewNumbered"), c.isStoreFunc(fn, "UpdateFrom"), c.isStoreFunc(fn, "DDL"):
	default:
		return
	}
	if isString(c.info.TypeOf(n.Args[0])) {
		c.requireConst(n.Args[0], key, fn.Name())
	}
}

// requireConst reports e, SQL text, unless it is built from constants.
func (c *checker) requireConst(e ast.Expr, key, where string) {
	if c.constSQL(e) {
		return
	}
	c.sqlReport(e, ruleSQLConst, key, c.sql.constant, "%s SQL text of %s in %s is not built from constants alone: pass values as ? parameters, and identifiers chosen at run time through a fixed map as clause.Column / clause.OrderByColumn", ruleSQLConst, where, key)
}

// sqlIndexFiles records, for constSQL, every value given to a variable of
// the package and every call of its functions.
func (c *checker) sqlIndexFiles(files []*ast.File) {
	x := &sqlIndex{values: map[*types.Var][]ast.Expr{}, ranges: map[*types.Var][]ast.Expr{}, calls: map[*types.Func][]*ast.CallExpr{},
		escapes: map[*types.Func]bool{}, varMemo: map[*types.Var]int8{}, funcMemo: map[string]int8{}, bound: map[*types.Var]bool{}}
	c.sqlx = x
	varOf := func(e ast.Expr) *types.Var {
		id, ok := ast.Unparen(e).(*ast.Ident)
		if !ok {
			return nil
		}
		obj := c.info.Defs[id]
		if obj == nil {
			obj = c.info.Uses[id]
		}
		v, _ := obj.(*types.Var)
		return v
	}
	called := map[*ast.Ident]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if ix, isIndex := ast.Unparen(l).(*ast.IndexExpr); isIndex {
						l = ix.X // an element written: the list holds an unknown value
						if v := varOf(l); v != nil {
							x.values[v] = append(x.values[v], nil)
						}
						continue
					}
					v := varOf(l)
					if sel, isSel := ast.Unparen(l).(*ast.SelectorExpr); isSel {
						v = fieldOf(c.info, sel)
					}
					if v == nil {
						continue
					}
					var rhs ast.Expr
					if len(n.Lhs) == len(n.Rhs) && (n.Tok == token.ASSIGN || n.Tok == token.DEFINE || n.Tok == token.ADD_ASSIGN) {
						rhs = n.Rhs[i]
					}
					if _, isIndex := ast.Unparen(n.Rhs[0]).(*ast.IndexExpr); isIndex && len(n.Rhs) == 1 && i == 0 && n.Tok != token.ADD_ASSIGN {
						rhs = n.Rhs[0] // v, ok := m[k]
					}
					x.values[v] = append(x.values[v], rhs)
				}
			case *ast.ValueSpec:
				for i, id := range n.Names {
					v := varOf(id)
					switch {
					case v == nil:
					case len(n.Values) == 0:
						// the zero value, "" for a string
					case len(n.Values) == len(n.Names):
						x.values[v] = append(x.values[v], n.Values[i])
					default:
						x.values[v] = append(x.values[v], nil)
					}
				}
			case *ast.RangeStmt:
				if v := varOf(n.Value); v != nil {
					x.ranges[v] = append(x.ranges[v], n.X)
				}
			case *ast.CompositeLit:
				st, ok := c.info.TypeOf(n).Underlying().(*types.Struct)
				for i, el := range n.Elts {
					if !ok {
						break
					}
					if kv, keyed := el.(*ast.KeyValueExpr); keyed {
						if id, isID := kv.Key.(*ast.Ident); isID {
							if f, isVar := c.info.Uses[id].(*types.Var); isVar && f.IsField() {
								x.values[f] = append(x.values[f], kv.Value)
							}
						}
					} else if i < st.NumFields() {
						x.values[st.Field(i)] = append(x.values[st.Field(i)], el)
					}
				}
			case *ast.UnaryExpr:
				if sel, isSel := ast.Unparen(n.X).(*ast.SelectorExpr); isSel && n.Op == token.AND {
					if f := fieldOf(c.info, sel); f != nil {
						x.values[f] = append(x.values[f], nil)
					}
				}
				if v := varOf(n.X); v != nil && n.Op == token.AND {
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
				if fn, ok := c.info.Uses[id].(*types.Func); ok && id != nil {
					called[id] = true
					x.calls[fn.Origin()] = append(x.calls[fn.Origin()], n)
				}
				for i, a := range n.Args {
					if v := varOf(a); v != nil && isList(v.Type()) && writesArg(c.info, n, i) {
						x.values[v] = append(x.values[v], nil) // handed over: the callee may write it
					}
				}
			}
			return true
		})
	}
	for id, obj := range c.info.Uses {
		if fn, ok := obj.(*types.Func); ok && !called[id] {
			x.escapes[fn.Origin()] = true
		}
	}
}

// constSQL reports whether e, a string, is built from constants alone: a
// constant; + of such; a variable every value of which is such (a range
// over a literal list of them included); a parameter of a function of the
// package that every call passes such (or, while a call's results are
// looked at, the call's argument); a call of a function of the package
// whose every result is such; fmt.Sprintf, strings.Join, Repeat and the
// trims of such; a conversion of such; a quoting helper (sqlQuoting); the
// TableName of a generated model; an element of a map or slice literal of
// such that nothing writes.
func (c *checker) constSQL(e ast.Expr) bool {
	e = ast.Unparen(e)
	if tv, ok := c.info.Types[e]; ok && tv.Value != nil {
		return true
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		return x.Op == token.ADD && c.constSQL(x.X) && c.constSQL(x.Y)
	case *ast.Ident:
		v, _ := c.info.Uses[x].(*types.Var)
		return v != nil && c.constVar(v)
	case *ast.CallExpr:
		return c.constCall(x)
	case *ast.SelectorExpr:
		return c.constField(x)
	case *ast.IndexExpr:
		// An element of a map or slice variable whose every value is a
		// literal of constants and no element of which is written.
		v, _ := c.info.Uses[identOf(x.X)].(*types.Var)
		return v != nil && isList(v.Type()) && c.constVarWith(v, c.constList)
	}
	return false
}

// writesArg reports whether call may write the elements of its argument i:
// any function may, of the builtins only copy, delete and clear the first.
func writesArg(info *types.Info, call *ast.CallExpr, i int) bool {
	id := identOf(call.Fun)
	if b, ok := info.Uses[id].(*types.Builtin); ok && id != nil {
		return i == 0 && (b.Name() == "copy" || b.Name() == "delete" || b.Name() == "clear")
	}
	return true
}

// isList reports a map, slice or array type.
func isList(t types.Type) bool {
	switch t.Underlying().(type) {
	case *types.Map, *types.Slice, *types.Array:
		return true
	}
	return false
}

// identOf is e as an identifier, or nil.
func identOf(e ast.Expr) *ast.Ident {
	id, _ := ast.Unparen(e).(*ast.Ident)
	return id
}

// constField reports a field read x.f whose every value is built from
// constants: an unexported field of the package, every value a
// literal or an assignment gives it in the package being one; or the SQL
// of a clause.Expr or NamedExpr, which every literal of them must give
// constant text (and gorm.Expr's argument too).
func (c *checker) constField(sel *ast.SelectorExpr) bool {
	s := c.info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal {
		return false
	}
	v, _ := s.Obj().(*types.Var)
	if v.Name() == "SQL" && (isNamed(s.Recv(), clausePkg, "Expr") || isNamed(s.Recv(), clausePkg, "NamedExpr")) {
		return true
	}
	// Only an unexported field: decoders (encoding/json, GORM's Scan) set
	// exported ones by reflection, past every write the index sees.
	return !v.Exported() && v.Pkg() != nil && strings.TrimPrefix(v.Pkg().Path(), c.module+"/") == c.rel && c.constVar(v)
}

// constList reports a list of strings built from constants: a literal of
// such, a variable whose every value is one, append of such.
func (c *checker) constList(e ast.Expr) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				el = kv.Value
			}
			if !c.constSQL(el) {
				return false
			}
		}
		return true
	case *ast.Ident:
		v, _ := c.info.Uses[x].(*types.Var)
		return v != nil && c.constVarWith(v, c.constList)
	case *ast.CallExpr:
		if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && id.Name == "append" && len(x.Args) > 0 {
			if _, builtin := c.info.Uses[id].(*types.Builtin); builtin {
				if !c.constList(x.Args[0]) {
					return false
				}
				for i, a := range x.Args[1:] {
					if x.Ellipsis.IsValid() && i == len(x.Args)-2 {
						return c.constList(a)
					}
					if !c.constSQL(a) {
						return false
					}
				}
				return true
			}
		}
	}
	return false
}

func (c *checker) constVar(v *types.Var) bool { return c.constVarWith(v, c.constSQL) }

// constVarWith reports whether every value of v satisfies ok.
func (c *checker) constVarWith(v *types.Var, ok func(ast.Expr) bool) bool {
	x := c.sqlx
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
	if c.isParam(v) {
		res = c.constParam(v)
	}
	for _, val := range x.values[v] {
		res = res && val != nil && ok(val)
	}
	for _, r := range x.ranges[v] {
		res = res && c.constList(r)
	}
	if res && len(x.values[v]) == 0 && len(x.ranges[v]) == 0 && !c.isParam(v) && !c.declaredHere(v) && !v.IsField() {
		res = false // declared elsewhere (another package, a field): unknown
	}
	if res {
		x.varMemo[v] = memoYes
	} else {
		x.varMemo[v] = memoNo
	}
	return res
}

// declaredHere reports a variable declared in the checked package.
func (c *checker) declaredHere(v *types.Var) bool {
	return v.Pkg() != nil && strings.TrimPrefix(v.Pkg().Path(), c.module+"/") == c.rel && !v.IsField()
}

// isParam reports a parameter of a function declared in the package, and
// that function.
func (c *checker) isParam(v *types.Var) bool { return c.paramFunc(v) != nil }

func (c *checker) paramFunc(v *types.Var) *ast.FuncDecl {
	for _, decl := range c.funcs {
		if decl.Type.Params == nil || !(decl.Type.Params.Pos() <= v.Pos() && v.Pos() < decl.Type.Params.End()) {
			continue
		}
		return decl
	}
	return nil
}

// constParam reports a parameter of a function of the package that is only
// ever called, and called with constant text at its position.
func (c *checker) constParam(v *types.Var) bool {
	decl := c.paramFunc(v)
	fn, _ := c.info.Defs[decl.Name].(*types.Func)
	if fn == nil || c.sqlx.escapes[fn] {
		return false
	}
	idx := paramIndex(c.info, decl, v)
	sig := fn.Type().(*types.Signature)
	if idx < 0 || sig.Variadic() && idx == sig.Params().Len()-1 {
		return false
	}
	calls := c.sqlx.calls[fn]
	for _, call := range calls {
		if idx >= len(call.Args) || !c.constSQL(call.Args[idx]) { // a spread only passes the variadic parameter
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
func (c *checker) constCall(call *ast.CallExpr) bool {
	if tv, ok := c.info.Types[call.Fun]; ok && tv.IsType() {
		return len(call.Args) == 1 && c.constSQL(call.Args[0]) // a conversion
	}
	if lit, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
		return len(call.Args) == 0 && c.constReturns(lit.Body)
	}
	fn := calledFunc(c.info, call)
	if fn == nil || call.Ellipsis.IsValid() && !(fn.Pkg() != nil && fn.Pkg().Path() == "strings" && fn.Name() == "Join") {
		return false
	}
	if fn.Pkg() != nil {
		if _, ok := c.sql.quoting[strings.TrimPrefix(fn.Pkg().Path(), c.module+"/")+"."+fn.Name()]; ok {
			return true
		}
	}
	switch path := pkgPath(fn); {
	case path == c.module+"/internal/store/model" && fn.Name() == "TableName":
		return true // generated: the table's name as a literal
	case path == "fmt" && fn.Name() == "Sprintf", path == "strings" && trimFuncs[fn.Name()]:
		for _, a := range call.Args {
			if !c.constSQL(a) {
				return false
			}
		}
		return true
	case path == "strings" && fn.Name() == "Join":
		return len(call.Args) == 2 && c.constList(call.Args[0]) && c.constSQL(call.Args[1])
	case path == "strings" && fn.Name() == "Repeat":
		return len(call.Args) == 2 && c.constSQL(call.Args[0]) // the count only repeats constant text
	}
	return c.constResult(fn, call)
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

// constResult reports a function of the package whose every return is
// built from constants, its parameters standing for the call's arguments.
func (c *checker) constResult(fn *types.Func, call *ast.CallExpr) bool {
	fn = fn.Origin()
	decl := c.funcs[fn]
	if decl == nil || decl.Body == nil || decl.Type.Results == nil || len(decl.Type.Results.List) != 1 {
		return false
	}
	// The arguments, as the parameters see them.
	bound := map[*types.Var]bool{}
	i := 0
	var sig strings.Builder
	sig.WriteString(fn.FullName())
	for _, field := range decl.Type.Params.List {
		for _, name := range field.Names {
			if v, ok := c.info.Defs[name].(*types.Var); ok && i < len(call.Args) && isString(v.Type()) {
				bound[v] = c.constSQL(call.Args[i])
				if bound[v] {
					sig.WriteString("+")
				} else {
					sig.WriteString("-")
				}
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	key := sig.String()
	switch c.sqlx.funcMemo[key] {
	case memoVisiting, memoYes:
		return true
	case memoNo:
		return false
	}
	c.sqlx.funcMemo[key] = memoVisiting
	saved := c.sqlx.bound
	c.sqlx.bound = bound
	savedMemo := c.sqlx.varMemo
	c.sqlx.varMemo = map[*types.Var]int8{} // what a local holds depends on the bound parameters
	res := c.constReturns(decl.Body)
	c.sqlx.bound, c.sqlx.varMemo = saved, savedMemo
	if res {
		c.sqlx.funcMemo[key] = memoYes
	} else {
		c.sqlx.funcMemo[key] = memoNo
	}
	return res
}

// --- the rules, on fixtures ----------------------------------------------

func TestSQLRules(t *testing.T) {
	root, _ := moduleRoot(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, goList(t, root, "-export", "-deps", "fmt", "strings", gormPkg, clausePkg, fixtureModule+"/internal/store"))
	policy := &sqlPolicy{
		raw:      map[string]string{"internal/billing.allowedRaw": "test"},
		constant: map[string]string{"internal/billing.allowedText": "test"},
		quoting:  map[string]string{"internal/store.QuoteIdent": "test"},
		pending:  map[string]string{"internal/plugin": "test"},
		used:     map[string]bool{},
	}
	r := rules{module: fixtureModule, sql: policy}
	cases := []struct {
		name, pkg, file, src string
		want                 []string
	}{
		{name: "raw-sql/Raw, Exec and NewNumbered, called or as values", pkg: "internal/billing", src: `
var stmt = store.NewNumbered("UPDATE t SET a = $1")

func f(db *gorm.DB) {
	var n int
	db.Raw("SELECT 1").Scan(&n)
	db.Exec("UPDATE t SET a = 1")
	exec := db.Exec
	_ = exec
}`, want: []string{ruleRawSQL, ruleRawSQL, ruleRawSQL, ruleRawSQL}},
		{name: "raw-sql/set operations over builder queries pass", pkg: "internal/billing", src: `
const keys = "(?) UNION (?) UNION (?)"

func f(db *gorm.DB, a, b *gorm.DB) {
	var n []int
	db.Raw("(?) UNION ALL (?) ORDER BY 1", a, b).Scan(&n)
	db.Raw(keys, a, b, a).Scan(&n)
	db.Raw("(?) UNION ALL (?) LIMIT 1", a, b).Scan(&n)
}`, want: []string{ruleRawSQL}},
		{name: "raw-sql/the allowlist, the builder and test files", pkg: "internal/billing", src: `
func allowedRaw(db *gorm.DB) error { return db.Exec("SELECT nextval(?)", "s").Error }

func f(db *gorm.DB) error {
	return db.Table("t").Where("id = ?", 1).Updates(map[string]any{"a": gorm.Expr("a + ?", 1)}).Error
}`},
		{name: "raw-sql/a test file", pkg: "internal/billing", file: "x_test.go", src: `
func f(db *gorm.DB) error { return db.Exec("UPDATE t SET a = 1").Error }`},
		{name: "raw-sql/a pending package is logged", pkg: "internal/plugin/x", src: `
func f(db *gorm.DB) error { return db.Exec("UPDATE t SET a = 1").Error }`},
		{name: "sql-const/text built from constants passes", pkg: "internal/billing", src: `
const cols = "a, b"

var where = "a = ? AND " + cols + " IS NOT NULL"

func filter(col string) string { return col + " = ?" }

func order(db *gorm.DB, desc bool) *gorm.DB {
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	return db.Order("a" + dir)
}

func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

func f(db *gorm.DB, x int) {
	db.Table("t").Select(cols).Where(where, x).Where(filter("b"), x).Group("a").Order(clause.OrderByColumn{Column: clause.Column{Name: "a"}})
	db.Where(fmt.Sprintf("%s = ?", "a"), x)
	db.Where(strings.Join([]string{"a = ?", "b = ?"}, " AND "), x, x)
	db.Where("a IN (" + strings.TrimSuffix(strings.Repeat("?,", x), ",") + ")")
	for _, t := range []string{"t1", "t2"} {
		db.Table(t)
	}
	db.Table(store.QuoteIdent(fmt.Sprint(x)))
	by(db, "a")
	by(db, "b")
	order(db, true)
	_ = gorm.Expr("a + ?", x)
	_ = clause.Expr{SQL: "a = ?", Vars: []any{x}}
	var c store.Conds
	c.And("a = ?", x)
}`},
		{name: "sql-const/text built from values fails", pkg: "internal/billing", src: `
func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

func f(db *gorm.DB, col string, x int, cols []string, m map[string]string) {
	db.Order(col)
	db.Where(fmt.Sprintf("a = %d", x))
	db.Where("a = " + fmt.Sprint(x))
	db.Select(strings.Join(cols, ","))
	db.Order(m[col])
	by(db, col)
	_ = gorm.Expr(col)
	_ = clause.Expr{SQL: col}
	var c store.Conds
	c.And(col)
	s := "a"
	s += col
	db.Group(s)
}`, want: []string{ruleSQLConst, ruleSQLConst, ruleSQLConst, ruleSQLConst, ruleSQLConst, ruleSQLConst,
			ruleSQLConst, ruleSQLConst, ruleSQLConst, ruleSQLConst}},
		{name: "sql-const/a function used as a value proves nothing of its parameters", pkg: "internal/billing", src: `
func by(db *gorm.DB, col string) *gorm.DB { return db.Where(col+" = ?", 1) }

var byFn = by

func f(db *gorm.DB) { by(db, "a") }`, want: []string{ruleSQLConst}},
		{name: "sql-const/the allowlist and a test file", pkg: "internal/billing", file: "x_test.go", src: `
func f(db *gorm.DB, col string) { db.Order(col) }`},
		{name: "sql-const/an allowed declaration", pkg: "internal/billing", src: `
func allowedText(db *gorm.DB, col string) { db.Order(col) }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\n" + fixtureImports + "import (\n\t\"fmt\"\n\t\"strings\"\n)\n\nvar (\n\t_ = fmt.Sprint\n\t_ = strings.Join\n)\n" + c.src + fixtureUses
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
			policy.pendingOut = nil
			findings := r.check(fset, path, []*ast.File{f}, info)
			var got []string
			for _, finding := range findings {
				got = append(got, strings.Fields(finding[strings.Index(finding, " [")+1:])[0])
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("rules %v, want %v\n%s", got, c.want, strings.Join(findings, "\n"))
			}
			if strings.Contains(c.name, "pending") && len(policy.pendingOut) == 0 {
				t.Fatal("the pending package's finding was not kept apart")
			}
		})
	}
	for _, key := range []string{ruleRawSQL + " internal/billing.allowedRaw", ruleSQLConst + " internal/billing.allowedText"} {
		if !policy.used[key] {
			t.Errorf("%s allowed nothing", key)
		}
	}
}

// verify fails on allowlist entries that allowed nothing and on packages
// of the pending and quoting lists that are none of the module's, and logs
// the findings of the pending packages (go test -v lists them).
func (p *sqlPolicy) verify(t *testing.T, known map[string]bool) {
	t.Helper()
	for _, entry := range p.unused() {
		t.Errorf("%s is listed but has no finding to allow; drop its entry", entry)
	}
	for pkg := range p.pending {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(known)), func(k string) bool { return within(k, pkg) }) {
			t.Errorf("pending %s holds no package of the module; drop its entry", pkg)
		}
	}
	for key := range p.quoting {
		if pkg := key[:strings.LastIndex(key, ".")]; !known[pkg] {
			t.Errorf("quoting helper %s: %s is no package of the module; drop its entry", key, pkg)
		}
	}
	for _, finding := range p.pendingOut {
		t.Logf("pending: %s", finding)
	}
}

// constReturns reports a function body each of whose returns is one
// string built from constants.
func (c *checker) constReturns(body *ast.BlockStmt) bool {
	res := true
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			res = res && len(n.Results) == 1 && c.constSQL(n.Results[0])
		}
		return res
	})
	return res
}

// fieldOf is the struct field a selector reads or writes, if any.
func fieldOf(info *types.Info, sel *ast.SelectorExpr) *types.Var {
	if s := info.Selections[sel]; s != nil && s.Kind() == types.FieldVal {
		v, _ := s.Obj().(*types.Var)
		return v
	}
	return nil
}

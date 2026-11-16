// Package gormguard holds the source checks that keep the core's database
// access in one shape: package store owns the pool, its GORM instance and
// the transactions, every statement is built and run by GORM, pgx is only
// the driver under database/sql, and Casbin only reads the policy. The checks are tests (in the style of
// the ticker and permission guards), type-checked so that they see through
// aliases, embedding and variables. Package store is the owner below;
// internal/testutil, test infrastructure, is one as well, and
// internal/store/gormtest for the callback recording its DryRun
// statements (instanceOwners in guard_test.go gives the reasons).
//
//   - [gorm-tx] outside the owners, no gorm (*DB).Transaction, Begin,
//     Commit, Rollback, SavePoint, RollbackTo, Connection, AutoMigrate or
//     Migrator: transactions start and end in store.DB.Tx and TxOptions,
//     connections come from WithConn (on which only package store opens a
//     transaction, the migrator's), and the schema comes from the SQL
//     migrations;
//   - [gorm-open] no gorm.Open outside the owners: handles come from
//     store.DB.Gorm and store.G;
//   - [gorm-instance] outside the owners, no (*DB).DB, Callback or Use, no
//     assignment to the instance's configuration (any gorm.Config field,
//     promoted into gorm.DB or not, DB.Config itself) and no use at all of
//     a ConnPool (a Statement's or the configuration's, read or written):
//     every handle shares store.Open's one instance, so such a change
//     would reach every statement of the process, and a statement run on
//     the ConnPool under a handle bypasses GORM and the rules here. Nor
//     (*DB).Debug, or a gorm.Session that sets (in a literal or by
//     assignment) PrepareStmt, AllowGlobalUpdate, Logger,
//     SkipDefaultTransaction or DryRun: such a session overrides that
//     configuration for its statements - prepared statements run on the
//     pool past sqlpool's result reading, an UPDATE or DELETE without
//     WHERE goes through, the SQL is logged with its values;
//   - [gorm-slice] no slice or array argument that GORM expands into a
//     list: any element type but byte, unless the type is a driver.Valuer
//     (store.Int64s, store.Strings and store.Times are one array parameter
//     each). This covers the parameters of Where, Or, Not, Having, Raw,
//     Exec, Joins, Assign and Attrs, the inline conditions of Find, First,
//     Take, Last, Delete, FirstOrInit and FirstOrCreate, the value of
//     Update and UpdateColumn, the parameters of Select when its first
//     argument is an SQL fragment (Select([]string{"id"}) and
//     Select("id", "name") list columns), and the arguments of gorm.Expr
//     and of store.Conds.And (which hands them to Where), and the Vars of
//     clause.Expr and clause.NamedExpr literals. A []any argument is
//     expanded as well: its elements become a list, so
//     Where("x IN ?", []any{1, 2}) is "x IN ($1,$2)". A spread (args...)
//     passes its elements and is not checked.
//     A byte slice ([]byte, json.RawMessage) is one bytea or json value,
//     except where its ? directly follows "(" in the SQL text: there
//     clause.Expr.Build expands every slice that is no driver.Valuer, so
//     Exec("INSERT INTO t (doc) VALUES (?)", raw) binds one parameter per
//     byte, and an empty raw becomes NULL. When that text is a constant
//     (a literal, a named constant, constants joined with +), the rule
//     finds the argument each ? takes and reports a byte slice at a "(?";
//     wrap it in store.Bytes, a driver.Valuer, or bind it through
//     gorm.Expr("?", raw). A "( ?" is safe, and SQL built at run time
//     (store.Numbered, text joined with variables) is not checked;
//   - [gorm-raw-unrun] no Error or RowsAffected read off a Raw statement
//     that no method ran (Scan, Row, Rows, Find ...): Raw only builds the
//     statement, so db.Raw("UPDATE ...").Error is nil and nothing is
//     written; a write takes Exec. The rule follows the statement through
//     a chain of gorm methods (db.Raw(...).Where(...)), parentheses, a
//     local variable (q := db.Raw(...) and later q.Error, when nothing but
//     field reads mentions q in between: a q.Scan, a call or closure that
//     takes q may have run it) and a call of a function of the same
//     package that returns one *gorm.DB, a Raw statement from every
//     return. It does not follow branches or loops (the last assignment
//     before the read is the last one in the text), struct fields, package
//     variables, or functions of other packages;
//   - [sql-handle] store.DB.SQL, the pool as a database/sql handle, only in
//     package store, internal/task (River's riverdatabasesql driver takes
//     it), internal/testutil and the test files (sqlOwners): production
//     statements run on store.DB.Gorm. Selectors (db.SQL, promoted ones
//     included) and store.DB literal keys are checked, and so is
//     store.SQLTx, the database/sql transaction under a handle (River's
//     InsertTx takes it);
//   - [tx-pool] no store.DB in a function literal passed to store.DB's Tx,
//     TxOptions or WithConn or to gorm's Transaction or Connection: the
//     callback holds a transaction or connection of the pool and runs its
//     statements on the handle it is given. A statement on the pool (s.db,
//     s.db.Gorm, a store.DB in a local variable) takes a second connection
//     while holding one, so a pool that runs out waits forever, and does
//     not see the transaction's writes. Each reference is reported once;
//     what a called function does with the pool is not followed, so a
//     function that may run inside a transaction takes the handle (q
//     *gorm.DB) instead of reading s.db itself;
//   - [tx-lock] a function (not a test's) that takes a *gorm.DB and runs
//     a statement with a lock held until the transaction ends - a row lock
//     (store.ForUpdate, store.Lock, a clause.Locking literal), SQL text
//     with FOR UPDATE / NO KEY UPDATE / SHARE / KEY SHARE or an
//     *_xact_lock call, or a call of a lock builder of its package (a
//     function that returns such a *gorm.DB) - calls store.RequireTx: on
//     the pool the lock would end with its statement. The handle's type no
//     longer says "transaction" as pgx.Tx did, so the check stands in for
//     it. A function that returns a *gorm.DB only builds the statement and
//     is not reported; its caller is. A check under the condition that
//     takes the lock (if lock { RequireTx }) counts;
//   - [testutil-import] no Go file but the test files and internal/testutil
//     itself imports internal/testutil: it is an owner because it is test
//     infrastructure, which production code never reaches;
//   - [pgx-import] no Go file of the module, test files and files left out
//     by build constraints included, imports pgx (any package of its
//     module) but internal/store/pgerror.go (pgxFiles), which reads the
//     SQLSTATE and constraint name of the *pgconn.PgError that GORM's
//     driver returns;
//   - [casbin-api] Casbin only reads the policy (CONTRACTS §99): the core
//     writes casbin_rule with GORM in the business transaction
//     (internal/authz/casbinrule). gorm-adapter is imported by
//     internal/authz/casbinx alone (casbinOwners; test files included).
//     Nowhere, casbinx included, is a write, transaction or self-reloading
//     API of Casbin's enforcers, its persist interfaces or gorm-adapter
//     named: policy management (Add/Remove/Update/Delete of policies,
//     grouping policies, roles, permissions and users, their Ctx, Ex and
//     Self forms), SavePolicy, Transaction, BeginTransaction,
//     WithTransaction, Commit, Rollback, StartAutoLoadPolicy, the synced,
//     transactional and distributed enforcers, nor EnableAutoSave with
//     anything but the constant false. gorm-adapter's API is a whitelist
//     (adapterAllowed): TurnOffAutoMigrate, LoadFilteredPolicy and
//     NewAdapterByDBUseTableName, the last only called on a variable that
//     an earlier TurnOffAutoMigrate of the same function ran on (otherwise
//     it AutoMigrates the table, narrowing v1 and v2); Open, GetDb,
//     LoadPolicy, the other constructors and the rest are reported. The
//     in-memory model (model.Model's AddPolicies, how a snapshot is built)
//     and the role manager are not covered. Casbin's own Enforce* is called
//     in casbinx alone: it runs every policy through the matcher, so a
//     request answers with PermissionSet.Has. Packages that depend on
//     Casbin are checked as well as those that depend on GORM.
//
// Why empty lists matter (gorm v1.31; gormtest's TestExpansionTraps keeps
// the shapes): GORM turns an expanded empty list into NULL, and the result
// depends on the form. Not("id IN ?", ids) with an empty ids is
// "NOT id IN (NULL)", NULL for every row, so no row matches where "not in
// the empty list" means all of them; Not("id", ids) is "NOT IN ()", invalid
// SQL. The rule above rejects both. What it cannot see are slices inside a
// map condition, whose values are only known at run time:
// Where(map[string]any{"id": ids}) is "id IN (NULL)", no row, as meant, and
// Not(map[string]any{"id": ids}) becomes "id IS NOT NULL", every row but
// those with a NULL id. Write list conditions with = ANY and an array
// parameter instead: Not("id = ANY(?)", store.Int64s(ids)) means what it
// says for every ids.
//
// The package has no code of its own; see guard_test.go, rules_test.go and
// casbin_test.go.
package gormguard

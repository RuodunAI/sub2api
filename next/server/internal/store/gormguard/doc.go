// Package gormguard holds the source checks that keep GORM in the role the
// migration gives it: building SQL and mapping columns, never owning a
// connection, a transaction or the schema. The checks are tests (in the
// style of the ticker and permission guards), type-checked so that they see
// through aliases, embedding and variables:
//
//   - [gorm-tx] no gorm (*DB).Transaction, Begin, AutoMigrate or Migrator:
//     transactions start in store.DB.Tx and the schema comes from the SQL
//     migrations;
//   - [gorm-open] no gorm.Open outside package store: handles come from
//     store.G;
//   - [gorm-instance] outside package store, no (*DB).DB, Callback or Use
//     and no assignment to the instance's configuration (any gorm.Config
//     field, promoted into gorm.DB or not, DB.Config itself, a ConnPool):
//     every handle shares store.G's one instance, so such a change would
//     reach every statement of the process;
//   - [gorm-slice] no slice or array argument that GORM expands into a
//     list: any element type but byte, unless the type is a driver.Valuer
//     (store.Int64s, store.Strings and store.Times are one array parameter
//     each; []byte and json.RawMessage are one value). This covers the
//     parameters of Where, Or, Not, Having, Raw, Exec, Joins, Assign and
//     Attrs, the inline conditions of Find, First, Take, Last, Delete,
//     FirstOrInit and FirstOrCreate, the value of Update and UpdateColumn,
//     the parameters of Select when its first argument is an SQL fragment
//     (Select([]string{"id"}) and Select("id", "name") list columns), and
//     the arguments of gorm.Expr and the Vars of clause.Expr and
//     clause.NamedExpr literals. A []any argument is expanded as well: its
//     elements become a list, so Where("x IN ?", []any{1, 2}) is
//     "x IN ($1,$2)". A spread (args...) passes its elements and is not
//     checked;
//   - [pgx-direct] in a package listed as migrated, no direct pgx Exec,
//     Query or QueryRow (a pgx.Batch and CopyFrom stay on pgx), except a
//     statement with an allow-pgx comment.
//
// A statement that a benchmark shows must stay on pgx (a hot path, such as
// the API key lookup) carries this comment on its line or the line above;
// the reason is required:
//
//	//gormguard:allow-pgx <reason>
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
// The package has no code of its own; see guard_test.go and rules_test.go.
package gormguard

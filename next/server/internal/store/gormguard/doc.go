// Package gormguard holds the source checks that keep GORM in the role the
// migration gives it: building SQL and mapping columns, never owning a
// connection, a transaction or the schema. The checks are tests (in the
// style of the ticker and permission guards), type-checked so that they see
// through aliases, embedding and variables:
//
//   - no gorm (*DB).Transaction, Begin, AutoMigrate or Migrator: transactions
//     start in store.DB.Tx and the schema comes from the SQL migrations;
//   - no gorm.Open outside package store: handles come from store.G;
//   - no plain []int64, []int32, []int or []string argument to Where, Or,
//     Not, Having, Raw, Exec, Joins, the inline conditions of Find, First,
//     Take, Last and Delete, or gorm.Expr: GORM expands such a slice into a
//     list (and an empty one into NULL); store.Int64s and store.Strings are
//     one array parameter;
//   - in a package listed as migrated, no direct pgx Exec, Query or QueryRow
//     (a pgx.Batch and CopyFrom stay on pgx).
//
// The package has no code of its own; see guard_test.go.
package gormguard

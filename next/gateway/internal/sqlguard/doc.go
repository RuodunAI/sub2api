// Package sqlguard holds the source checks that keep the gateway's SQL in
// one shape, in the spirit of the core's gormguard: every statement of the
// controller is built by GORM (models and the query builder), and the SQL
// text GORM is handed never carries a value. The checks are tests,
// type-checked so that they see through aliases and constants, over every
// non-test Go file of the module.
//
//   - [raw-sql] no (*gorm.DB).Raw or Exec: a statement is a model's
//     Create, Updates, Delete or a query of the builder (Table, Where,
//     Joins, Select, Clauses ...). What GORM cannot express stays in the
//     function rawAllowed lists, with the reason: the schema script, a
//     data-modifying CTE, the catalog lookup and the EXISTS combinator.
//   - [sql-const] the SQL text handed to GORM is a constant: the first
//     argument of Raw, Exec, Where, Not, Or, Having, Select, Distinct,
//     Order, Group, Joins, Table and Pluck when it is a string (or a
//     []string literal's elements), the SQL of gorm.Expr, and the SQL field
//     of a clause.Expr or clause.NamedExpr literal. A constant may be
//     joined from constants with +; a value goes in as a ? parameter, and
//     an identifier taken from input goes through a fixed map to a
//     clause.Column or clause.OrderByColumn, never fmt.Sprintf or + with a
//     variable. The functions constAllowed lists build their text at run
//     time from constants only, with the reason.
//
// The package has no code of its own; see guard_test.go.
package sqlguard

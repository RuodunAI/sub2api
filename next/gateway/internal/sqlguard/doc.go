// Package sqlguard holds the source check that keeps the gateway's SQL in
// one shape, the core's and the plugins' alike: every statement of the
// controller is built by GORM (models and the query builder), and the SQL
// text GORM is handed never carries a value. The rules, [raw-sql] and
// [sql-const], are runtime-contract/gormpg/sqlcheck's; policy (in
// guard_test.go) lists what the gateway keeps written out, with the
// reason: the schema script and a data-modifying CTE.
//
// The package has no code of its own; see guard_test.go.
package sqlguard

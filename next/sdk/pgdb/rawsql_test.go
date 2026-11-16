package pgdb_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg/sqlcheck"
)

// sqlPolicy is the SDK's and the plugins' ([raw-sql], [sql-const];
// runtime-contract/gormpg/sqlcheck): keys are "package.Decl" relative to
// next/. Everywhere else a statement is built by GORM from the models
// (CONTRACTS §2, "插件数据库"), and SQL text is built from constants: values
// go in as ? parameters, and identifiers chosen by a request through a fixed
// whitelist into clause.Column, clause.Table or clause.OrderByColumn. The
// combinators (EXISTS, INSERT ... SELECT, nextval, the advisory lock) are
// gormpg's, which pgdb names for the plugins.
var sqlPolicy = sqlcheck.Policy{
	Module: "github.com/Sub2API-Devs/sup2api/next",
	Raw: map[string]string{
		"sdk/pgdb.LockTable":                              "LOCK TABLE: no data statement",
		"sdk/pgdb/pgdbtool.readCatalog":                   "schema drift check: reads pg_catalog (columns, constraints, indexes)",
		"sdk/pgdb/pgdbtool.readOthers":                    "schema drift check: reads pg_catalog (views, sequences, functions, triggers, types)",
		"sdk/pgdb/pgdbtool.comparer.sameDefault":          "schema drift check: compares two default expressions in the server's own casts",
		"sdk/pgdb/pgdbtool.driftMigrator.hasModelDefault": "schema drift check: reads a column default from pg_attrdef and normalizes one through a temporary table under a savepoint (DDL)",
		"sdk/pgdb/pgdbtool.searchPath":                    "migration replay: SET LOCAL search_path",
		"sdk/pgdb/pgdbtool.newScratch":                    "migration replay: the scratch schema and role (DDL)",
		"sdk/pgdb/pgdbtool.scratch.migrate":               "migration replay: SET LOCAL ROLE, the migration files, RESET ROLE",
		"sdk/pgdb/pgdbtool.scratch.outside":               "migration replay: the catalog read of the role's grants",
		"sdk/pgdb/pgdbtool.ApplyMigrations":               "migration runner: SET LOCAL search_path and the migration files (DDL)",
		"sdk/pgdb/pgdbtool.CreateSchema":                  "migration runner: CREATE SCHEMA",
		"sdk/pgdb/pgdbtool.DropSchema":                    "migration runner: DROP SCHEMA",
	},
	Dynamic: map[string]string{
		"sdk/pgdb/pgdbtool.comparer.sameDefault":          "two default expressions read from pg_catalog (never input) cast to a type format_type printed",
		"sdk/pgdb/pgdbtool.driftMigrator.hasModelDefault": "the CREATE TEMPORARY TABLE of a default probe: the type and default read from pg_catalog / the model, never input",
		"sdk/pgdb/pgdbtool.scratch.migrate":               "replays the migration files of the plugin being packed",
		"sdk/pgdb/pgdbtool.ApplyMigrations":               "runs the migration files of the plugin being migrated",
	},
	// SQL marks the fragments a plugin's helper hands on to GORM.
	ConstTypes: map[string]bool{"sdk/pgdb.SQL": true},
}

// TestNoRawSQL runs the SQL check over the non-test files of the SDK, the
// plugins and the tools. Like TestNoPgxImports, the plugins and the tools
// are checked when the SDK sits in the repository (next/go.work); alone,
// the SDK is checked by itself, where no plugin's helper takes a pgdb.SQL.
func TestNoRawSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks every plugin")
	}
	next, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	p, dir, patterns := sqlPolicy, next, []string(nil)
	if _, err := os.Stat(filepath.Join(next, "go.work")); err == nil {
		if patterns, err = sqlcheck.WorkModules(next, "sdk", "plugins", "tools"); err != nil {
			t.Fatal(err)
		}
	} else {
		p.ConstTypes, dir, patterns = nil, filepath.Join(next, "sdk"), []string{"./..."}
	}
	findings, err := sqlcheck.New(p).Scan(dir, patterns...)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

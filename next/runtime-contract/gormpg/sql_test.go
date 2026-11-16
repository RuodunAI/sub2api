package gormpg

import (
	"os"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg/sqlcheck"
)

// sqlPolicy allows the statements of query.go, which the core, the gateway
// and the SDK share ([raw-sql]), and Open's pool ([gorm-instance]); the
// rest of the module builds none.
var sqlPolicy = sqlcheck.Policy{
	Module: "github.com/Sub2API-Devs/sup2api/next/runtime-contract",
	Raw: map[string]string{
		"gormpg.ExistsQuery":           "thin combinator: SELECT EXISTS (q1), EXISTS (q2) ... over builder queries, in one round trip",
		"gormpg.InsertSelect":          "thin combinator: INSERT INTO t (cols) <builder query> [ON CONFLICT ...], which GORM's Create cannot express",
		"gormpg.InsertSelectReturning": "thin combinator: InsertSelect with a RETURNING, scanned into the caller's rows",
		"gormpg.NextIDs":               "nextval of a table's id sequence, for a batch insert that needs its ids first",
		"gormpg.RelationExists":        "to_regclass: a catalog lookup",
		"gormpg.TryXactLock":           "pg_try_advisory_xact_lock: a transaction-scoped advisory lock, without the wait",
	},
	Pool: map[string]string{"gormpg.Open": "installs sqlpool as the instance's connection pool"},
}

// TestNoRawSQL runs the SQL check (package sqlcheck) over the module.
func TestNoRawSQL(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := sqlcheck.New(sqlPolicy).Scan(dir+"/..", "./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

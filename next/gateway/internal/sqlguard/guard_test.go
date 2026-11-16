package sqlguard

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg/sqlcheck"
)

const gatewayModule = "github.com/Sub2API-Devs/sup2api/next/gateway"

// policy is the gateway's: the declarations that keep a statement written
// out ([raw-sql]) or reach the pool under a handle ([gorm-instance]), with
// the reason. The combinators it shares with the core (EXISTS, to_regclass) are
// runtime-contract/gormpg's.
var policy = sqlcheck.Policy{
	Module: gatewayModule,
	Raw: map[string]string{
		"internal/control.execLocked":      "DDL: the shell schema script behind its transaction-scoped advisory lock, one simple-protocol statement",
		"internal/control.Store.Heartbeat": "data-modifying CTE: the conditional UPDATE and the identity check in one round trip per heartbeat; GORM cannot put an UPDATE in a WITH",
	},
	Pool: map[string]string{
		"cmd/sub2api-gateway.closePool": "closes the pool's connections at shutdown",
		"cmd/sub2api-gateway.newShell":  "logs the pool's size at start",
	},
}

// TestGatewaySQLRules runs the SQL check (runtime-contract/gormpg/sqlcheck)
// over every non-test file of the module's packages that depend on GORM.
func TestGatewaySQLRules(t *testing.T) {
	dir, err := sqlcheck.ModuleDir(".", gatewayModule)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := sqlcheck.New(policy).Scan(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

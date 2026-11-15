package testutil

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// errorsTB records Errorf instead of failing the test.
type errorsTB struct {
	testing.TB
	errs []string
}

func (e *errorsTB) Errorf(format string, args ...any) {
	e.errs = append(e.errs, fmt.Sprintf(format, args...))
}

// The cleanup of DB fails a test that leaves casbin_rule other than the
// projection of the role tables, naming the rows; an exact one passes.
func TestCheckCasbinDrift(t *testing.T) {
	db := DB(t)
	ctx := context.Background()
	rec := &errorsTB{TB: t}
	checkCasbinDrift(rec, DSN(db))
	if len(rec.errs) != 0 {
		t.Fatalf("a fresh database drifted: %v", rec.errs)
	}

	// casbin_rule has no row triggers: a direct write is drift.
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('g', 'u:42', 'r:ghost', '')`); err != nil {
		t.Fatal(err)
	}
	checkCasbinDrift(rec, DSN(db))
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "extra g u:42 r:ghost") {
		t.Fatalf("drift not reported: %v", rec.errs)
	}
	if _, err := db.SQL.ExecContext(ctx, `SELECT authz_casbin_repair()`); err != nil {
		t.Fatal(err)
	}
}

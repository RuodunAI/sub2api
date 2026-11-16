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

// The cleanup of DB fails a test that leaves casbin_rule rows naming a
// deleted user, role or permission, naming the rows; a fresh database
// passes.
func TestCheckCasbinOrphans(t *testing.T) {
	db := DB(t)
	ctx := context.Background()
	rec := &errorsTB{TB: t}
	checkCasbinOrphans(rec, DSN(db))
	if len(rec.errs) != 0 {
		t.Fatalf("a fresh database has orphans: %v", rec.errs)
	}

	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO roles (key, name) VALUES ('ghost', '{}');
INSERT INTO permissions (key, module, label, source) VALUES ('account:read', 'account', '{}', 'core');
INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('g', 'u:42', 'r:ghost', ''), ('p', 'r:ghost', 'account:read', '*')`); err != nil {
		t.Fatal(err)
	}
	checkCasbinOrphans(rec, DSN(db))
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "g u:42 r:ghost") || strings.Contains(rec.errs[0], "p r:ghost") {
		t.Fatalf("orphans not reported, or more than them: %v", rec.errs)
	}
	if _, err := db.SQL.ExecContext(ctx, `DELETE FROM casbin_rule WHERE v0 = 'u:42'`); err != nil {
		t.Fatal(err)
	}
}

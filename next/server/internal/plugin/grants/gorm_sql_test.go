package grants

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the grant statements built with GORM (see package gormtest).

var by = new(int64(7))

func TestSQLGrantsGranted(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []scopeRow
		return grantedRows(db, "demo", "1.2.0", []string{"kv", "lock"}).Scan(&rows)
	})
}

// The per-call check (Holds), with a scope and without one (any scope).
func TestSQLGrantsHolds(t *testing.T) {
	for name, scope := range map[string][]byte{"scope": []byte(`{"prefix":"a"}`), "any": nil} {
		t.Run(name, func(t *testing.T) {
			var ok bool
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return holdsQuery(db, "demo", "1.2.0", "kv", scope).Scan(&ok) })
		})
	}
}

func TestSQLGrantsRecord(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "active"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			gormtest.AssertRun(t, func(db *gorm.DB) error {
				return Record(db.Statement.Context, db, "demo", "1.2.0", "hash", by, staged,
					[]Decision{{Permission: "kv", Scope: []byte(`{"prefix":"a"}`), Status: "granted"}})
			})
		})
	}
}

// Promote and Withdraw take the staged set out with its rows (DryRun
// returns none, so the runs stop there).
func TestSQLGrantsPromote(t *testing.T) {
	gormtest.AssertRun(t, func(db *gorm.DB) error { return Promote(db.Statement.Context, db, "demo", "1.2.0") })
}

func TestSQLGrantsWithdraw(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		gormtest.AssertRun(t, func(db *gorm.DB) error { return Withdraw(db.Statement.Context, db, "demo", "1.2.0", "failed") })
	})
	// plugin_versions has no updated_at: only consent_status moves.
	t.Run("await_consent", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return awaitConsent(db, "demo", "1.2.0") })
	})
}

func TestSQLGrantsRevoke(t *testing.T) {
	gormtest.AssertRun(t, func(db *gorm.DB) error {
		_, err := Revoke(db.Statement.Context, db, "demo", "kv", by)
		return err
	})
}

// Narrow and Reinstate lock the staged rows they read.
func TestSQLGrantsStagedScopes(t *testing.T) {
	for name, run := range map[string]func(context.Context, *gorm.DB) error{
		"narrow": func(ctx context.Context, db *gorm.DB) error {
			return Narrow(ctx, db, "demo", "kv", map[string]any{"prefix": "a"}, by)
		},
		"reinstate": func(ctx context.Context, db *gorm.DB) error {
			return Reinstate(ctx, db, "demo", "kv", map[string]any{"prefix": "a"}, by)
		},
	} {
		t.Run(name, func(t *testing.T) {
			gormtest.AssertRun(t, func(db *gorm.DB) error { return run(db.Statement.Context, db) })
		})
	}
}

func TestSQLGrantsGrantStaged(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return grantStaged(db, "demo", "kv", []string{"1.2.0", "1.3.0"}, []byte(`{"prefix":"a"}`), by)
	})
}

func TestSQLGrantsVersionGrants(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var rows []scopeRow
		return VersionGrantsQuery(db, "demo", "1.2.0").Scan(&rows)
	})
}

func TestSQLGrantsRevoked(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var perms []string
		return revoked(db, "demo").Scan(&perms)
	})
}

package grants

import (
	"regexp"
	"slices"
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

// The per-call check (Holds), with a scope and without one (NULL).
func TestSQLGrantsHolds(t *testing.T) {
	for name, scope := range map[string][]byte{"scope": []byte(`{"prefix":"a"}`), "any": nil} {
		t.Run(name, func(t *testing.T) {
			var ok bool
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return holdsQuery(db, "demo", "1.2.0", "kv", scope).Scan(&ok) })
		})
	}
}

// holdsQuery passes the key and version in the order VersionRows numbers
// them.
func TestVersionRowsPlaceholderOrder(t *testing.T) {
	got := regexp.MustCompile(`\$\d`).FindAllString(VersionRows, -1)
	if want := []string{"$1", "$2", "$1", "$1", "$2"}; !slices.Equal(got, want) {
		t.Fatalf("VersionRows placeholders %v, holdsQuery passes %v", got, want)
	}
}

func TestSQLGrantsRecord(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "active"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
				return db.Exec(recordSQL(staged), "demo", "1.2.0", "hash", by, "kv", []byte(`{"prefix":"a"}`), "granted")
			})
		})
	}
}

func TestSQLGrantsPromote(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Exec(promoteSQL, "demo", "1.2.0") })
}

func TestSQLGrantsWithdraw(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			var n int
			return withdrawn(db, "demo", "1.2.0").Scan(&n)
		})
	})
	// plugin_versions has no updated_at: only consent_status moves.
	t.Run("await_consent", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return awaitConsent(db, "demo", "1.2.0") })
	})
}

func TestSQLGrantsRevoke(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		var n int
		return revoke(db, "demo", "kv", by).Scan(&n)
	})
}

// Narrow and Reinstate lock the staged rows they read.
func TestSQLGrantsStagedScopes(t *testing.T) {
	for name, sel := range map[string]string{"narrow": narrowedSQL, "reinstate": reinstatedSQL} {
		t.Run(name, func(t *testing.T) {
			gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
				var rows []scopeRow
				return db.Raw(sel, map[string]any{"key": "demo", "permission": "kv"}).Scan(&rows)
			})
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

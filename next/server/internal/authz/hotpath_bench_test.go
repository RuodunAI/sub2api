package authz

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The reads behind every authorization check: a request whose user's
// permission set is not cached at the current authz version (any authz
// change drops the cache) loads it (loadPermissionSet, after the catalog,
// ensureCatalog), and the version poll and committedLater read the version
// (currentVersion). Measured against a real PostgreSQL (TEST_DATABASE_URL),
// as recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/authz/
func BenchmarkHotAuthz(b *testing.B) {
	ctx := context.Background()
	db := testutil.DB(b)
	s := New(Deps{DB: db})
	if err := s.Start(ctx); err != nil {
		b.Fatal(err)
	}
	var uid int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('bench@test.invalid', 'x') RETURNING id`).Scan(&uid); err != nil {
		b.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx pgx.Tx) error {
		_, err := s.SetUserRoles(ctx, tx, 0, uid, []string{RoleAdmin})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct {
		name string
		run  func() error
	}{
		// An admin: one row per granted permission.
		{"permission_set", func() error {
			set, err := s.loadPermissionSet(ctx, uid)
			if err == nil && len(set.Keys) == 0 {
				b.Fatal("empty permission set")
			}
			return err
		}},
		{"catalog", func() error {
			s.mu.Lock()
			s.catalog = nil
			s.mu.Unlock()
			return s.ensureCatalog(ctx)
		}},
		{"version", func() error {
			_, err := s.currentVersion(ctx)
			return err
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if err := bc.run(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := bc.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

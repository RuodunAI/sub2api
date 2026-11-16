package authz

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The reads behind authorization: none on a request (the node's Casbin
// enforcer decides in memory); a write of one user's roles or status reads
// that user (user_roles) on every node, a policy write reads the policy
// (policy, with every active user's g rows) on every node, a superuser's
// /me reads the active permissions (ActivePermissionKeys), and the version
// poll and committedLater read the versions (currentVersions). Measured
// against a real PostgreSQL (TEST_DATABASE_URL), as recorded in
// internal/store/gormtest/BASELINE.md (permission_set there: the per-user
// read the enforcer replaced):
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
	if err := db.SQL.QueryRowContext(ctx, `INSERT INTO users (email, password_hash) VALUES ('bench@test.invalid', 'x') RETURNING id`).Scan(&uid); err != nil {
		b.Fatal(err)
	}
	if err := db.Tx(ctx, func(tx *gorm.DB) error {
		_, err := s.SetUserRoles(ctx, tx, 0, uid, []string{RoleAdmin})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	loader := casbinx.NewLoader(db)
	for _, bc := range []struct {
		name string
		run  func() error
	}{
		// An admin's g rows, as a node reads them after a write of them.
		{"user_roles", func() error {
			ur, err := loader.UserRoles(ctx, uid)
			if err == nil && len(ur.Roles) == 0 {
				b.Fatal("no roles read")
			}
			return err
		}},
		{"policy", func() error {
			_, err := loader.Policy(ctx)
			return err
		}},
		{"active_permissions", func() error {
			_, err := s.ActivePermissionKeys(ctx)
			return err
		}},
		{"version", func() error {
			_, _, err := s.currentVersions(ctx)
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

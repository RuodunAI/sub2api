package iam

import (
	"context"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The reads that authenticate console requests: the session user's status
// and token version (isActiveWithVersion, on a status cache miss), the
// access key of a request made with one (VerifyAccessKey, not cached), and
// the user GET /me, a login and a refresh load (getUser). Each GORM
// statement is measured next to the pgx statement it replaced, against a
// real PostgreSQL (TEST_DATABASE_URL), as recorded in
// internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/iam/

// hotFixture is a user with a role and an access key; it returns the user
// and the key's hash.
func hotFixture(b *testing.B) (*Service, int64, string) {
	ctx := context.Background()
	db := testutil.DB(b)
	user := testutil.InsertUser(b, db, "bench@test.invalid")
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO roles (key, name) VALUES ('bench', '{"en":"Bench"}')`); err != nil {
		b.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO user_roles (user_id, role_id) SELECT $1, id FROM roles WHERE key = 'bench'`, user); err != nil {
		b.Fatal(err)
	}
	hash := hashToken("bench-key")
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO user_access_keys (user_id, name, token_hash, token_hint, permissions, last_used_at)
		VALUES ($1, 'k', $2, 'abcd', '{account:read:own,user:read:all}', now())`, user, hash); err != nil {
		b.Fatal(err)
	}
	return New(Deps{DB: db}), user, hash
}

// benchPaths runs the raw database/sql (formerly pgx) and the GORM form of one read.
func benchPaths(b *testing.B, sqlPath, gormPath func() error) {
	for _, p := range []struct {
		name string
		run  func() error
	}{{"sql", sqlPath}, {"gorm", gormPath}} {
		b.Run(p.name, func(b *testing.B) {
			if err := p.run(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := p.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHotUserStatus(b *testing.B) {
	s, user, _ := hotFixture(b)
	ctx := context.Background()
	var active bool
	var version int
	benchPaths(b, func() error {
		return s.db.SQL.QueryRowContext(ctx, `SELECT status = 'active', token_version FROM users WHERE id = $1 AND deleted_at IS NULL`, user).Scan(&active, &version)
	}, func() error {
		return userStatus(store.G(ctx, s.db.Gorm), user).Row().Scan(&active, &version)
	})
}

func BenchmarkHotAccessKey(b *testing.B) {
	s, _, hash := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		var id core.AccessKeyIdentity
		var expiresAt, lastUsed *time.Time
		return s.db.SQL.QueryRowContext(ctx, `SELECT id, user_id, permissions, expires_at, last_used_at FROM user_access_keys WHERE token_hash = $1`, hash).
			Scan(&id.KeyID, &id.UserID, &id.Permissions, &expiresAt, &lastUsed)
	}, func() error {
		var id core.AccessKeyIdentity
		var expiresAt, lastUsed *time.Time
		return accessKeyByHash(store.G(ctx, s.db.Gorm), hash).Row().
			Scan(&id.KeyID, &id.UserID, (*store.Strings)(&id.Permissions), &expiresAt, &lastUsed)
	})
}

func BenchmarkHotGetUser(b *testing.B) {
	s, user, _ := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		_, err := sqlGetUser(ctx, s.db, user)
		return err
	}, func() error {
		_, err := s.getUser(ctx, s.db.Gorm, user)
		return err
	})
}

// sqlGetUser is getUser as it was on pgx, on database/sql.
func sqlGetUser(ctx context.Context, db *store.DB, id int64) (*User, error) {
	var u User
	var balance string
	err := db.SQL.QueryRowContext(ctx, `SELECT `+userColumns+` FROM `+usersFrom+` WHERE u.id = $1 AND u.deleted_at IS NULL`, id).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.Status, &u.LastLoginAt, &u.CreatedAt, &u.UpdatedAt,
			&u.Roles, &u.Segment.ID, &u.Segment.Kind, &u.Segment.Name, &u.Segment.UserID, &balance)
	u.Balance = &balance
	return &u, err
}

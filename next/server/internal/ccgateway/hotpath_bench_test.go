package ccgateway

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/secret"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// sharedAccountType is the only statement this package runs for every
// forwarded request without a cache in front (a configuration without
// per-account runtimes). It stays on pgx; the GORM form of the same statement
// is measured next to it, against a real PostgreSQL (TEST_DATABASE_URL), as
// recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/ccgateway/
func BenchmarkHotSharedAccountType(b *testing.B) {
	db := testutil.DB(b)
	ctx := context.Background()
	cipher, _ := secret.New(make([]byte, 32))
	s := New(db, cipher)
	var id int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO accounts(name,plugin_key,type,credentials_enc) VALUES('ccg','ccgateway','managed',''::bytea) RETURNING id`).Scan(&id); err != nil {
		b.Fatal(err)
	}
	for _, bc := range []struct {
		name string
		read func() (string, error)
	}{
		{"pgx", func() (string, error) { return s.sharedAccountType(ctx, id) }},
		{"gorm", func() (string, error) {
			var kind string
			err := store.G(ctx, db.Pool).Model(&model.Account{}).Select("type").
				Where("id = ? AND plugin_key = 'ccgateway'", id).Take(&kind).Error
			return kind, err
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if kind, err := bc.read(); err != nil || kind != "managed" {
				b.Fatalf("read: %q, %v", kind, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := bc.read(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

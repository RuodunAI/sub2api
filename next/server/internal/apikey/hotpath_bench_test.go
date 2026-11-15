package apikey

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The key lookup is the first database statement of every gateway request
// (Gateway.serve → Authenticate) and is not cached by design (see cached).
// Measured against a real PostgreSQL (TEST_DATABASE_URL) as the baseline
// the GORM migration is compared with (internal/store/gormtest/BASELINE.md):
//
//	go test -run '^$' -bench Hot -benchmem -count 5 ./internal/apikey/
func BenchmarkHotKeyLookup(b *testing.B) {
	ctx := context.Background()
	db := testutil.DB(b)
	user := testutil.InsertUser(b, db, "bench@test.invalid")
	group := testutil.InsertGroup(b, db, "bench")
	raw, err := generateKey()
	if err != nil {
		b.Fatal(err)
	}
	hash := HashKey(raw)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO api_keys (user_id, group_id, name, key_prefix, key_hash) VALUES ($1, $2, 'k', $3, $4)`,
		user, group, raw[:displayPrefix], hash); err != nil {
		b.Fatal(err)
	}
	svc := New(db, nil, nil, nil)
	if e, err := svc.lookup(ctx, hash); err != nil || !e.Found {
		b.Fatalf("lookup: %+v, %v", e, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := svc.lookup(ctx, hash); err != nil {
			b.Fatal(err)
		}
	}
}

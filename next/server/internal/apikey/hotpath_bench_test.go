package apikey

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The key lookup is the first database statement of every gateway request
// (Gateway.serve → Authenticate) and is not cached by design (see cached).
// Its GORM form (gorm, lookup) is measured next to the pgx statement it
// replaced (pgx), against a real PostgreSQL (TEST_DATABASE_URL), as
// recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/apikey/
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
	// pgxLookupSQL is lookupSQL as pgx ran it.
	pgxLookupSQL := strings.Replace(lookupSQL, "k.key_hash = ?", "k.key_hash = $1", 1)
	for _, bc := range []struct {
		name   string
		lookup func() (*cached, error)
	}{
		{"pgx", func() (*cached, error) {
			e := &cached{}
			var groupRate, discount decimal.Decimal
			var override *decimal.Decimal
			err := db.Pool.QueryRow(ctx, pgxLookupSQL, hash).Scan(
				&e.KeyID, &e.UserID, &e.KeyStatus, &e.ExpiresAt, &e.UserActive,
				&e.GroupID, &e.GroupName, &e.GroupStatus, &groupRate, &e.ModelAllowlist, &e.ModelFilterMode, &e.GroupEnvironment, &e.GroupAvailable, &e.AllowedIPs,
				&e.KeyLimits.RPM, &e.KeyLimits.TPM, &e.KeyLimits.Concurrency, &e.KeyQuota.Limit, &e.KeyQuota.Used, &e.GroupLossPolicy,
				&e.SegmentID, &discount, &override)
			e.Found = err == nil
			rate, source := core.SegmentRate(groupRate, discount, override)
			e.RateMultiplier, e.RateSource = rate.String(), source
			return e, err
		}},
		{"gorm", func() (*cached, error) { return svc.lookup(ctx, hash) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if e, err := bc.lookup(); err != nil || !e.Found {
				b.Fatalf("lookup: %+v, %v", e, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := bc.lookup(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

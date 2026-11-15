package helperhistory

import (
	"context"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// anyPrefix is the unlocked probe Lookup runs for every request that carries
// helper prefixes, most of which it answers Unknown. It stays on pgx; the
// GORM form of the same statement is measured next to it, against a real
// PostgreSQL (TEST_DATABASE_URL), as recorded in
// internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/helperhistory/
func BenchmarkHotHelperPrefix(b *testing.B) {
	s, r := fixture(b)
	finish(b, s, r, strings.Repeat("1", 64), "one") // the owner holds a chain
	ctx := context.Background()
	miss := []string{strings.Repeat("2", 64), strings.Repeat("3", 64)}
	for _, bc := range []struct {
		name  string
		probe func() (bool, error)
	}{
		{"pgx", func() (bool, error) { return s.anyPrefix(ctx, r.Owner, miss) }},
		{"gorm", func() (bool, error) {
			var found bool
			db := store.G(ctx, s.db.Pool)
			err := db.Raw("SELECT EXISTS(?)", withPrefixes(db, r.Owner, miss).Select("1")).Scan(&found).Error
			return found, err
		}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if found, err := bc.probe(); err != nil || found {
				b.Fatalf("probe: %v, %v", found, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := bc.probe(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

package helperhistory

import (
	"context"
	"strings"
	"testing"
)

// anyPrefix is the unlocked probe Lookup runs for every request that carries
// helper prefixes, most of which it answers Unknown. Its GORM statement
// (gorm) is measured next to the pgx statement it replaced (sql; raw database/sql since batch 6), against a
// real PostgreSQL (TEST_DATABASE_URL), as recorded in
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
		{"sql", func() (bool, error) {
			var found bool
			err := s.db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM provider_helper_records WHERE user_id=$1 AND group_id=$2 AND public_prefix_digest=ANY($3))`,
				r.Owner.UserID, r.Owner.GroupID, miss).Scan(&found)
			return found, err
		}},
		{"gorm", func() (bool, error) { return s.anyPrefix(ctx, r.Owner, miss) }},
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

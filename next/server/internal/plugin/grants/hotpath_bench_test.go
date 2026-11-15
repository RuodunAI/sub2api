package grants

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Holds runs for every HostService call a plugin instance makes that needs
// a host permission (hostServer.require), which plugin-backed gateway
// requests do on their way. Its GORM statement (gorm) is measured next to
// the pgx statement it replaced (pgx), against a real PostgreSQL
// (TEST_DATABASE_URL), as recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/plugin/grants/
func BenchmarkHotGrantHolds(b *testing.B) {
	ctx := context.Background()
	db := testutil.DB(b)
	for _, sql := range []string{
		`INSERT INTO plugins (key, name, status) VALUES ('bench', '{"en":"bench"}', 'enabled')`,
		`INSERT INTO plugin_permission_grants (plugin_key, permission, scope, status, plugin_version, manifest_hash)
			VALUES ('bench', 'kv', '{"prefix":"a"}', 'granted', '1.0.0', 'x')`,
	} {
		if _, err := db.Pool.Exec(ctx, sql); err != nil {
			b.Fatal(err)
		}
	}
	scope := []byte(`{"prefix":"a"}`)
	for _, bc := range []struct {
		name  string
		holds func() (bool, error)
	}{
		{"pgx", func() (bool, error) {
			var ok bool
			err := db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+VersionRows+`
				WHERE permission = $3 AND status = 'granted' AND ($4::jsonb IS NULL OR scope = $4::jsonb))`, "bench", "1.0.0", "kv", scope).Scan(&ok)
			return ok, err
		}},
		{"gorm", func() (bool, error) { return Holds(ctx, db.Pool, "bench", "1.0.0", "kv", scope) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			if ok, err := bc.holds(); err != nil || !ok {
				b.Fatalf("holds: %v, %v", ok, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := bc.holds(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

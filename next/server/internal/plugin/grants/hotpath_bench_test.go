package grants

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Holds runs for every HostService call a plugin instance makes that needs
// a host permission (hostServer.require), which plugin-backed gateway
// requests do on their way. It stays on pgx; the GORM form of the same
// statement is measured next to it, against a real PostgreSQL
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
		{"pgx", func() (bool, error) { return Holds(ctx, db.Pool, "bench", "1.0.0", "kv", scope) }},
		{"gorm", func() (bool, error) {
			var ok bool
			err := store.G(ctx, db.Pool).Raw(`SELECT EXISTS (SELECT 1 FROM `+versionRows+`
				WHERE permission = @permission AND status = 'granted' AND (CAST(@scope AS jsonb) IS NULL OR scope = CAST(@scope AS jsonb)))`,
				map[string]any{"key": "bench", "version": "1.0.0", "permission": "kv", "scope": string(scope)}).Row().Scan(&ok)
			return ok, err
		}},
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

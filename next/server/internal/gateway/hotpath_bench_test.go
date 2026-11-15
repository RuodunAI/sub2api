package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The statements of the gateway's request path: the settings rows and the
// sticky rules. Both are cached (settingsTTL, rulesTTL), so a request
// runs them on a miss only. Each GORM statement (gorm) is measured next to
// the pgx statement it replaced (pgx), against a real PostgreSQL
// (TEST_DATABASE_URL), as recorded in internal/store/gormtest/BASELINE.md:
//
//	go test -run '^$' -bench Hot -benchmem -benchtime 1000x -count 5 ./internal/gateway/

// hotFixture holds the built-in sticky rules and a gateway settings row.
func hotFixture(b *testing.B) *store.DB {
	ctx := context.Background()
	db := testutil.DB(b)
	if err := db.Tx(ctx, func(tx pgx.Tx) error { return syncBuiltinRules(ctx, tx, builtinStickyRules()) }); err != nil {
		b.Fatal(err)
	}
	doc, _ := json.Marshal(defaultGatewaySettings())
	if _, err := db.Pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)`, settingsKeyGateway, doc); err != nil {
		b.Fatal(err)
	}
	return db
}

// benchPaths runs the former pgx and the GORM form of one read.
func benchPaths(b *testing.B, pgxPath, gormPath func() error) {
	for _, p := range []struct {
		name string
		run  func() error
	}{{"pgx", pgxPath}, {"gorm", gormPath}} {
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

func BenchmarkHotSettingsRow(b *testing.B) {
	db := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		v := defaultGatewaySettings()
		var raw []byte
		if err := db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, settingsKeyGateway).Scan(&raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &v)
	}, func() error {
		_, err := loadGatewaySettings(ctx, db.Pool)
		return err
	})
}

func BenchmarkHotStickyRules(b *testing.B) {
	db := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		rows, err := db.Pool.Query(ctx, `SELECT `+ruleColumns+` FROM sticky_rules ORDER BY priority, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var out []*stickyRule
		for rows.Next() {
			var r stickyRule
			var match, sources, includes []byte
			if err := rows.Scan(&r.ID, &r.Name, &r.Source, &r.Enabled, &r.Priority, &match, &sources,
				&r.ValueRegex, &r.TTLSeconds, &includes, &r.OnFailure, &r.UpdatedBy, &r.UpdatedAt, &r.Customized, &r.PassHeaders); err != nil {
				return err
			}
			r.decodeDocuments(match, sources, includes)
			out = append(out, &r)
		}
		return rows.Err()
	}, func() error {
		_, err := listRules(ctx, db.Pool)
		return err
	})
}

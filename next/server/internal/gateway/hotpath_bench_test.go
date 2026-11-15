package gateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// The statements of the gateway's request path: the settings rows and the
// sticky rules. Both are cached (settingsTTL, rulesTTL), so a request
// runs them on a miss only; they stay on pgx. Each is measured next to the
// GORM form of the same statement, against a real PostgreSQL
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

// benchPaths runs the pgx and the GORM form of one read.
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
		_, err := loadGatewaySettings(ctx, db.Pool)
		return err
	}, func() error {
		v := defaultGatewaySettings()
		var row model.Setting
		err := store.G(ctx, db.Pool).Model(&model.Setting{}).Select("value").Where("key = ?", settingsKeyGateway).Take(&row).Error
		if err != nil {
			return err
		}
		return json.Unmarshal(row.Value, &v)
	})
}

func BenchmarkHotStickyRules(b *testing.B) {
	db := hotFixture(b)
	ctx := context.Background()
	benchPaths(b, func() error {
		_, err := listRules(ctx, db.Pool)
		return err
	}, func() error {
		var rows []model.StickyRule
		if err := store.G(ctx, db.Pool).Order("priority, id").Find(&rows).Error; err != nil {
			return err
		}
		out := make([]*stickyRule, 0, len(rows))
		for i := range rows {
			out = append(out, ruleOf(&rows[i]))
		}
		return nil
	})
}

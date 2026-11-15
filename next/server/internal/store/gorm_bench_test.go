package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// benchAccount is a typical list row: ints, text, a nullable id, jsonb,
// text[] and a timestamp.
type benchAccount struct {
	ID        int64
	Name      string
	Status    string
	Priority  int
	CreatedBy *int64
	Settings  []byte
	Models    store.Strings
	CreatedAt time.Time
}

func (benchAccount) TableName() string { return "accounts" }

const benchCols = `id, name, status, priority, created_by, settings, models, created_at`

func scanBenchAccount(r interface{ Scan(...any) error }, a *benchAccount) error {
	return r.Scan(&a.ID, &a.Name, &a.Status, &a.Priority, &a.CreatedBy, &a.Settings, &a.Models, &a.CreatedAt)
}

func benchAccounts(b *testing.B) (*store.DB, int64) {
	if testing.Short() {
		b.Skip("skipping database benchmark in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(b)
	var id int64
	for range 20 {
		require.NoError(b, db.SQL.QueryRowContext(ctx, `INSERT INTO accounts (name, plugin_key, type, credentials_enc, settings, models)
			VALUES ('bench', 'p', 't', '\x00', '{"region": "us", "tier": 2}', '{model-a,model-b}') RETURNING id`).Scan(&id))
	}
	return db, id
}

func BenchmarkByID(b *testing.B) {
	db, id := benchAccounts(b)
	ctx := context.Background()
	b.Run("sql", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var a benchAccount
			if err := scanBenchAccount(db.SQL.QueryRowContext(ctx, `SELECT `+benchCols+` FROM accounts WHERE id = $1`, id), &a); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var a benchAccount
			if err := store.G(ctx, db.Gorm).Take(&a, id).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkList20(b *testing.B) {
	db, _ := benchAccounts(b)
	ctx := context.Background()
	b.Run("sql", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, err := db.SQL.QueryContext(ctx, `SELECT `+benchCols+` FROM accounts ORDER BY id LIMIT 20`)
			if err != nil {
				b.Fatal(err)
			}
			var list []benchAccount
			for rows.Next() {
				var a benchAccount
				if err := scanBenchAccount(rows, &a); err != nil {
					b.Fatal(err)
				}
				list = append(list, a)
			}
			err = rows.Close()
			if err != nil || len(list) != 20 {
				b.Fatal(len(list), err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var list []benchAccount
			if err := store.G(ctx, db.Gorm).Order("id").Limit(20).Find(&list).Error; err != nil || len(list) != 20 {
				b.Fatal(len(list), err)
			}
		}
	})
}

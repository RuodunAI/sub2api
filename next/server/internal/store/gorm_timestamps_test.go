package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// stampRow is tagged as the generator tags such a table: created_at has no
// database default, updated_at defaults to now().
type stampRow struct {
	ID        int64     `gorm:"column:id;primaryKey;default:(-)"`
	Name      string    `gorm:"column:name"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;default:(-);autoUpdateTime:false"`
}

func (stampRow) TableName() string { return "stamp_rows" }

const stampTable = `CREATE TABLE stamp_rows (
	id bigserial PRIMARY KEY,
	name text NOT NULL,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now())`

var stampAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// The statements the timestamp hooks build, without a database.
func TestSnapshotTimestamps(t *testing.T) {
	for name, build := range map[string]func(*gorm.DB) *gorm.DB{
		// created_at = now() and read back with the defaulted columns.
		"insert": func(tx *gorm.DB) *gorm.DB { return tx.Create(&stampRow{Name: "a"}) },
		// A created_at that is set is written as is.
		"insert_explicit": func(tx *gorm.DB) *gorm.DB { return tx.Create(&stampRow{Name: "a", CreatedAt: stampAt}) },
		"insert_batch": func(tx *gorm.DB) *gorm.DB {
			return tx.Create(&[]stampRow{{Name: "a"}, {Name: "b", CreatedAt: stampAt}})
		},
		// An UPDATE writes updated_at only when it assigns it.
		"update": func(tx *gorm.DB) *gorm.DB { return tx.Model(&stampRow{ID: 1}).Update("name", "b") },
		"updates_struct": func(tx *gorm.DB) *gorm.DB {
			return tx.Model(&stampRow{ID: 1}).Updates(stampRow{Name: "b"})
		},
		"updates_explicit": func(tx *gorm.DB) *gorm.DB {
			return tx.Model(&stampRow{ID: 1}).Updates(map[string]any{"name": "b", "updated_at": stampAt})
		},
		"updates_now": func(tx *gorm.DB) *gorm.DB {
			return tx.Model(&stampRow{ID: 1}).Updates(map[string]any{"name": "b", "updated_at": gorm.Expr("now()")})
		},
		"update_column": func(tx *gorm.DB) *gorm.DB { return tx.Model(&stampRow{ID: 1}).UpdateColumn("name", "b") },
		"update_table": func(tx *gorm.DB) *gorm.DB {
			return tx.Table("stamp_rows").Where("id = ?", 1).Update("name", "b")
		},
	} {
		t.Run(name, func(t *testing.T) { gormtest.Assert(t, build) })
	}
}

// On a database: both stamps are the transaction's now(), as the raw SQL
// gets, the INSERT reads created_at back into the struct, and an UPDATE
// moves updated_at only when it assigns it.
func TestGORMTimestamps(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	_, err := db.SQL.ExecContext(ctx, stampTable)
	require.NoError(t, err)

	inTx := func(f func(g *gorm.DB) error) time.Time {
		t.Helper()
		var now time.Time
		require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
			if err := store.SQLTx(tx).QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
				return err
			}
			return f(store.G(ctx, tx))
		}))
		return now
	}
	read := func(id int64) stampRow {
		t.Helper()
		var r stampRow
		require.NoError(t, store.G(ctx, db.Gorm).First(&r, id).Error)
		return r
	}

	row := stampRow{Name: "a"}
	created := inTx(func(g *gorm.DB) error { return g.Create(&row).Error })
	assert.True(t, created.Equal(row.CreatedAt), "created_at read back: %v, now() %v", row.CreatedAt, created)
	got := read(row.ID)
	assert.True(t, created.Equal(got.CreatedAt))
	assert.True(t, created.Equal(got.UpdatedAt))

	inTx(func(g *gorm.DB) error { return g.Model(&stampRow{ID: row.ID}).Update("name", "b").Error })
	got = read(row.ID)
	assert.Equal(t, "b", got.Name)
	assert.True(t, created.Equal(got.UpdatedAt), "an UPDATE that does not assign updated_at leaves it alone")

	updated := inTx(func(g *gorm.DB) error {
		return g.Model(&stampRow{ID: row.ID}).Updates(map[string]any{"name": "c", "updated_at": gorm.Expr("now()")}).Error
	})
	got = read(row.ID)
	assert.True(t, updated.After(created))
	assert.True(t, updated.Equal(got.UpdatedAt), "updated_at %v, now() %v", got.UpdatedAt, updated)
	assert.True(t, created.Equal(got.CreatedAt))

	inTx(func(g *gorm.DB) error {
		return g.Model(&stampRow{ID: row.ID}).Updates(map[string]any{"name": "d", "updated_at": stampAt}).Error
	})
	assert.True(t, stampAt.Equal(read(row.ID).UpdatedAt), "an assigned updated_at wins")

	rows := []stampRow{{Name: "x"}, {Name: "y", CreatedAt: stampAt}}
	batch := inTx(func(g *gorm.DB) error { return g.Create(&rows).Error })
	assert.True(t, batch.Equal(rows[0].CreatedAt))
	assert.True(t, stampAt.Equal(rows[1].CreatedAt), "a created_at that is set is kept")
	assert.True(t, stampAt.Equal(read(rows[1].ID).CreatedAt))
}

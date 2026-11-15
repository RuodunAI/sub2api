package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

type doc struct {
	ID    int64                         `gorm:"column:id;primaryKey;default:(-)"`
	Tags  store.JSON[[]string]          `gorm:"column:tags"`
	Attrs *store.JSON[map[string]int64] `gorm:"column:attrs"`
}

func (doc) TableName() string { return "docs" }

// JSON[T] reads and writes jsonb as a T through GORM and through pgx.
func TestGORMJSON(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	_, err := db.Pool.Exec(ctx, `CREATE TABLE docs (id bigserial PRIMARY KEY, tags jsonb NOT NULL, attrs jsonb)`)
	require.NoError(t, err)
	g := store.G(ctx, db.Pool)

	in := doc{Tags: store.JSON[[]string]{V: []string{"a", "b"}}, Attrs: &store.JSON[map[string]int64]{V: map[string]int64{"n": 1}}}
	require.NoError(t, g.Create(&in).Error)
	var got doc
	require.NoError(t, g.First(&got, in.ID).Error)
	assert.Equal(t, in, got)

	// NULL: a nil pointer writes it and reads back; a value reads it as zero.
	empty := doc{}
	require.NoError(t, g.Create(&empty).Error)
	var back doc
	require.NoError(t, g.First(&back, empty.ID).Error)
	assert.Nil(t, back.Attrs)
	assert.Nil(t, back.Tags.V, "a nil slice is the JSON null")
	tags := store.JSON[[]string]{V: []string{"stale"}}
	require.NoError(t, g.Raw(`SELECT attrs FROM docs WHERE id = ?`, empty.ID).Scan(&tags).Error)
	assert.Nil(t, tags.V)
	require.NoError(t, g.Raw(`SELECT tags FROM docs WHERE id = ?`, in.ID).Scan(&tags).Error)
	assert.Equal(t, []string{"a", "b"}, tags.V)

	// As a parameter, and read by pgx directly.
	var n int64
	require.NoError(t, g.Raw(`SELECT count(*) FROM docs WHERE tags @> ?::jsonb`, store.JSON[[]string]{V: []string{"b"}}).Scan(&n).Error)
	assert.EqualValues(t, 1, n)
	var viaPgx store.JSON[[]string]
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT tags FROM docs WHERE id = $1`, in.ID).Scan(&viaPgx))
	assert.Equal(t, []string{"a", "b"}, viaPgx.V)
	viaPgx = store.JSON[[]string]{V: []string{"stale"}}
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT NULL::jsonb`).Scan(&viaPgx))
	assert.Nil(t, viaPgx.V)

	// It serializes as its value.
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ID": `+jsonInt(in.ID)+`, "Tags": ["a", "b"], "Attrs": {"n": 1}}`, string(b))
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Times is one timestamptz[] parameter and reads the column back.
func TestGORMTimes(t *testing.T) {
	db, _ := bridgeDB(t)
	g := store.G(context.Background(), db.Pool)
	at := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	var got store.Times
	require.NoError(t, g.Raw(`SELECT ?::timestamptz[] || ?::timestamptz[]`, store.Times{at}, store.Times(nil)).Row().Scan(&got))
	require.Len(t, got, 1)
	assert.True(t, at.Equal(got[0]))
	var n int
	require.NoError(t, g.Raw(`SELECT count(*) FROM unnest(?::timestamptz[]) WHERE unnest > ?`, store.Times{at, at.Add(time.Hour)}, at).Row().Scan(&n))
	assert.Equal(t, 1, n)
}

type loose struct {
	ID  int64 `gorm:"column:id;primaryKey;default:(-)"`
	Doc any   `gorm:"column:doc"`
}

func (loose) TableName() string { return "loose" }

// GORM refuses a model with a field of an interface type.
func TestGORMRefusesInterfaceFields(t *testing.T) {
	_, _, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&loose{Doc: map[string]any{"a": 1}})
	})
	assert.ErrorContains(t, err, "unsupported data type")
}

// nilRowsQuerier is a broken Querier whose Query returns neither rows nor
// an error.
type nilRowsQuerier struct{ store.Querier }

func (nilRowsQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }

func TestGORMNilRows(t *testing.T) {
	var n int
	err := store.G(context.Background(), nilRowsQuerier{}).Raw(`SELECT 1`).Scan(&n).Error
	assert.ErrorContains(t, err, "neither rows nor an error")
}

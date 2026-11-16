package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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

// JSON[T] reads and writes jsonb as a T through GORM and through
// database/sql.
func TestGORMJSON(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	_, err := db.SQL.ExecContext(ctx, `CREATE TABLE docs (id bigserial PRIMARY KEY, tags jsonb NOT NULL, attrs jsonb)`)
	require.NoError(t, err)
	g := store.G(ctx, db.Gorm)

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

	// As a parameter, and read by database/sql directly.
	var n int64
	require.NoError(t, g.Raw(`SELECT count(*) FROM docs WHERE tags @> ?::jsonb`, store.JSON[[]string]{V: []string{"b"}}).Scan(&n).Error)
	assert.EqualValues(t, 1, n)
	var viaSQL store.JSON[[]string]
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT tags FROM docs WHERE id = $1`, in.ID).Scan(&viaSQL))
	assert.Equal(t, []string{"a", "b"}, viaSQL.V)
	viaSQL = store.JSON[[]string]{V: []string{"stale"}}
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT NULL::jsonb`).Scan(&viaSQL))
	assert.Nil(t, viaSQL.V)

	// It serializes as its value.
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"ID": `+jsonInt(in.ID)+`, "Tags": ["a", "b"], "Attrs": {"n": 1}}`, string(b))
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// Times is one timestamptz[] parameter and reads the column back.
func TestGORMTimes(t *testing.T) {
	db, _ := widgetDB(t)
	g := store.G(context.Background(), db.Gorm)
	at := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)
	var got store.Times
	require.NoError(t, g.Raw(`SELECT ?::timestamptz[] || ?::timestamptz[]`, store.Times{at}, store.Times(nil)).Row().Scan(&got))
	require.Len(t, got, 1)
	assert.True(t, at.Equal(got[0]))
	var n int
	require.NoError(t, g.Raw(`SELECT count(*) FROM unnest(?::timestamptz[]) WHERE unnest > ?`, store.Times{at, at.Add(time.Hour)}, at).Row().Scan(&n))
	assert.Equal(t, 1, n)
}

// Bytes is one parameter right after "(", where GORM spreads a []byte into
// one placeholder per byte, and keeps nil (NULL) apart from empty.
func TestGORMBytes(t *testing.T) {
	db, _ := widgetDB(t)
	g := store.G(context.Background(), db.Gorm)
	for _, c := range []struct {
		in   store.Bytes
		want []byte
	}{{nil, []byte("stored")}, {store.Bytes{}, []byte{}}, {store.Bytes("ab"), []byte("ab")}} {
		var got []byte
		require.NoError(t, g.Raw(`SELECT COALESCE(?, 'stored'::bytea)`, c.in).Row().Scan(&got))
		assert.Equal(t, c.want, got, "COALESCE(%#v, 'stored')", c.in)
	}
	var doc store.Bytes
	require.NoError(t, g.Raw(`SELECT jsonb_build_object('a', 1)::text::bytea`).Row().Scan(&doc))
	assert.Equal(t, `{"a": 1}`, string(doc))
	var n int
	require.NoError(t, g.Raw(`SELECT count(*) FROM jsonb_object_keys((?::jsonb))`, store.Bytes(`{"a":1,"b":2}`)).Row().Scan(&n))
	assert.Equal(t, 2, n)
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

// NextIDs reserves ascending ids from the table's sequence; a batch created
// with them keeps them.
func TestNextIDs(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	_, err := db.SQL.ExecContext(ctx, `CREATE TABLE docs (id bigserial PRIMARY KEY, tags jsonb NOT NULL, attrs jsonb)`)
	require.NoError(t, err)
	g := store.G(ctx, db.Gorm)

	none, err := store.NextIDs(g, "docs", 0)
	require.NoError(t, err)
	assert.Empty(t, none)

	ids, err := store.NextIDs(g, "docs", 3)
	require.NoError(t, err)
	require.Len(t, ids, 3)
	assert.True(t, ids[0] < ids[1] && ids[1] < ids[2])
	rows := []doc{{ID: ids[0]}, {ID: ids[1]}, {ID: ids[2]}}
	require.NoError(t, g.Create(&rows).Error)
	var got []int64
	require.NoError(t, g.Model(&doc{}).Order("id").Pluck("id", &got).Error)
	assert.Equal(t, ids, got)
}

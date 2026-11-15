package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/schema"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestEveryModelStoresAndReadsValues writes rows into the table of every
// model and reads them back through store.G, so that each column's mapping
// and default meets real values: the -infinity default of
// provider_helper_usage_outbox.next_attempt_at, for one, which a LIMIT 0
// select never decodes. Each table gets two rows, one at a time:
//
//   - "defaults": only the NOT NULL columns without a default are set, so
//     the database fills the rest and RETURNING brings it back (stamped
//     created_at and updated_at are left to store's timestamp hook);
//   - "values": every writable column is set, NULLable ones included.
//
// The rows are value fixtures, not domain data. The test database drops its
// CHECK constraints, and each insert runs with session_replication_role =
// replica, which switches foreign keys and triggers off; so one generic
// filler fits every table and no table is skipped.
func TestEveryModelStoresAndReadsValues(t *testing.T) {
	db := testutil.DB(t)
	dropCheckConstraints(t, db)
	cols := readColumns(t, db)
	cache := &sync.Map{}

	for _, m := range model.All() {
		s, err := schema.Parse(m, cache, schema.NamingStrategy{})
		require.NoError(t, err)
		t.Run(s.Table, func(t *testing.T) {
			require.NotEmpty(t, cols[s.Table], "no columns read for %s", s.Table)
			for _, full := range []bool{false, true} {
				name := map[bool]string{false: "defaults", true: "values"}[full]
				t.Run(name, func(t *testing.T) { storeAndRead(t, db, s, cols[s.Table], full) })
			}
		})
	}
}

func storeAndRead(t *testing.T, db *store.DB, s *schema.Schema, cols map[string]columnInfo, full bool) {
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck // the rows are never kept
	_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DELETE FROM `+s.Table)
	require.NoError(t, err)

	row := reflect.New(s.ModelType)
	var stamped []*schema.Field
	for _, f := range s.Fields {
		c, ok := cols[f.DBName]
		require.True(t, ok, "field %s has no column", f.Name)
		switch {
		case c.generated || !f.Creatable:
		case !full && isStamped(f, c):
			stamped = append(stamped, f)
		case full || c.required():
			f.ReflectValueOf(ctx, row.Elem()).Set(sample(f.FieldType, c))
		}
	}
	g := store.G(ctx, tx)
	require.NoError(t, g.Create(row.Interface()).Error)
	for _, f := range stamped {
		v, zero := f.ValueOf(ctx, row.Elem())
		require.False(t, zero, "%s was not stamped", f.DBName)
		require.WithinDuration(t, time.Now(), v.(time.Time), time.Minute, f.DBName)
	}

	got := reflect.New(reflect.SliceOf(s.ModelType))
	require.NoError(t, g.Find(got.Interface()).Error)
	require.Equal(t, 1, got.Elem().Len())
	for _, f := range s.Fields {
		want, have := f.ReflectValueOf(ctx, row.Elem()), f.ReflectValueOf(ctx, got.Elem().Index(0))
		require.True(t, sameValue(want, have), "%s: wrote %v, read %v", f.DBName, want, have)
	}
}

type columnInfo struct {
	udt                 string
	maxLen              int
	notNull, hasDefault bool
	generated           bool
}

func (c columnInfo) required() bool { return c.notNull && !c.hasDefault && !c.generated }

// isStamped reports a column that store's insert hook fills.
func isStamped(f *schema.Field, c columnInfo) bool {
	return (f.DBName == "created_at" || f.DBName == "updated_at") && f.FieldType == reflect.TypeFor[time.Time]() && c.required()
}

func readColumns(t *testing.T, db *store.DB) map[string]map[string]columnInfo {
	rows, err := db.Pool.Query(context.Background(), `
		SELECT table_name, column_name, udt_name, coalesce(character_maximum_length, 0),
		       is_nullable = 'NO', column_default IS NOT NULL OR is_identity = 'YES', is_generated = 'ALWAYS'
		FROM information_schema.columns WHERE table_schema = 'public'`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]map[string]columnInfo{}
	for rows.Next() {
		var table, col string
		var c columnInfo
		require.NoError(t, rows.Scan(&table, &col, &c.udt, &c.maxLen, &c.notNull, &c.hasDefault, &c.generated))
		if out[table] == nil {
			out[table] = map[string]columnInfo{}
		}
		out[table][col] = c
	}
	require.NoError(t, rows.Err())
	return out
}

func dropCheckConstraints(t *testing.T, db *store.DB) {
	ctx := context.Background()
	rows, err := db.Pool.Query(ctx, `
		SELECT format('ALTER TABLE %s DROP CONSTRAINT %I', conrelid::regclass, conname)
		FROM pg_constraint WHERE contype = 'c' AND conislocal AND connamespace = 'public'::regnamespace`)
	require.NoError(t, err)
	var stmts []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		stmts = append(stmts, s)
	}
	require.NoError(t, rows.Err())
	for _, s := range stmts {
		_, err := db.Pool.Exec(ctx, s)
		require.NoError(t, err, s)
	}
}

var sampleTime = time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)

// sample is a non-zero value of type t for column c. Times keep
// microseconds, the database's precision; integers stay small enough for
// smallint and numeric(p, s).
func sample(t reflect.Type, c columnInfo) reflect.Value {
	if t.Kind() == reflect.Pointer {
		p := reflect.New(t.Elem())
		p.Elem().Set(sample(t.Elem(), c))
		return p
	}
	var v any
	switch t {
	case reflect.TypeFor[time.Time]():
		v = sampleTime
	case reflect.TypeFor[pgtype.Timestamptz]():
		v = pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}
	case reflect.TypeFor[decimal.Decimal]():
		v = decimal.NewFromInt(2)
	case reflect.TypeFor[json.RawMessage]():
		v = json.RawMessage(`{"k": "v"}`)
	case reflect.TypeFor[[]byte]():
		v = []byte{1, 2}
	case reflect.TypeFor[store.Int64s]():
		v = store.Int64s{1, 2}
	case reflect.TypeFor[store.Strings]():
		v = store.Strings{"a", "b"}
	default:
		v = sampleScalar(t.Kind(), c)
	}
	return reflect.ValueOf(v).Convert(t)
}

func sampleScalar(k reflect.Kind, c columnInfo) any {
	switch {
	case k == reflect.Bool:
		return true
	case k >= reflect.Int && k <= reflect.Int64:
		return 2
	case k == reflect.Float32 || k == reflect.Float64:
		return 1.5
	case k == reflect.String && c.udt == "uuid":
		return "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	case k == reflect.String && c.udt == "bpchar":
		return strings.Repeat("s", c.maxLen) // char(n) pads to n
	case k == reflect.String:
		return "s"
	}
	panic(fmt.Sprintf("no sample for %s", k))
}

// sameValue compares what was written with what was read: times and
// decimals by value, since the database answers in its own location and
// with the column's scale.
func sameValue(a, b reflect.Value) bool {
	if a.Kind() == reflect.Pointer {
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameValue(a.Elem(), b.Elem())
	}
	switch x := a.Interface().(type) {
	case time.Time:
		return x.Equal(b.Interface().(time.Time))
	case decimal.Decimal:
		return x.Equal(b.Interface().(decimal.Decimal))
	case pgtype.Timestamptz:
		y := b.Interface().(pgtype.Timestamptz)
		return x.Valid == y.Valid && x.InfinityModifier == y.InfinityModifier && x.Time.Equal(y.Time)
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

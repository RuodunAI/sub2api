package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

type widget struct {
	ID    int64
	Name  string
	Owner *int64
	Tag   string
	Tags  store.Strings
	Refs  store.Int64s
}

// bridgeDB is a migrated database with a widgets table for the bridge
// contract, plus one user id to refer to.
func bridgeDB(t testing.TB) (*store.DB, int64) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(t)
	_, err := db.Pool.Exec(ctx, `CREATE TABLE widgets (
		id bigserial PRIMARY KEY,
		name text NOT NULL UNIQUE,
		owner bigint REFERENCES users(id),
		tag text NOT NULL DEFAULT '',
		tags text[] NOT NULL DEFAULT '{}',
		refs bigint[] NOT NULL DEFAULT '{}')`)
	require.NoError(t, err)
	var uid int64
	require.NoError(t, db.Pool.QueryRow(ctx, `INSERT INTO users (email, password_hash) VALUES ('bridge@test', 'x') RETURNING id`).Scan(&uid))
	return db, uid
}

var errRollback = errors.New("rollback on purpose")

func countWidgets(t *testing.T, ctx context.Context, q store.Querier) (viaPgx, viaGORM int64) {
	t.Helper()
	require.NoError(t, q.QueryRow(ctx, `SELECT count(*) FROM widgets`).Scan(&viaPgx))
	require.NoError(t, store.G(ctx, q).Model(&widget{}).Count(&viaGORM).Error)
	return viaPgx, viaGORM
}

func TestGORMSharesThePgxTransaction(t *testing.T) {
	db, uid := bridgeDB(t)
	ctx := context.Background()

	// A transaction rolled back by defer, unlike DB.Tx, also when a require
	// below ends the test with runtime.Goexit.
	tx, err := db.Pool.Begin(ctx)
	require.NoError(t, err)
	func() {
		defer tx.Rollback(ctx) //nolint:errcheck
		g := store.G(ctx, tx)
		w := widget{Name: "a", Owner: &uid, Tags: store.Strings{"x", "y"}, Refs: store.Int64s{3, 4}}
		require.NoError(t, g.Create(&w).Error)
		require.NotZero(t, w.ID, "RETURNING id scanned back")

		// GORM -> pgx.
		var name string
		require.NoError(t, tx.QueryRow(ctx, `SELECT name FROM widgets WHERE id = $1`, w.ID).Scan(&name))
		assert.Equal(t, "a", name)

		// pgx -> GORM.
		_, err = tx.Exec(ctx, `INSERT INTO widgets (name, tag) VALUES ('b', 'k'), ('c', 'k')`)
		require.NoError(t, err)
		var got []widget
		require.NoError(t, g.Where("tag = ?", "k").Order("id").Find(&got).Error)
		assert.Equal(t, []string{"b", "c"}, []string{got[0].Name, got[1].Name})

		// Arrays round-trip and filter as one parameter each.
		var back widget
		require.NoError(t, g.Where("refs && ?", store.Int64s{4, 9}).Take(&back).Error)
		assert.Equal(t, store.Strings{"x", "y"}, back.Tags)
		assert.Equal(t, store.Int64s{3, 4}, back.Refs)
		var none []widget
		require.NoError(t, g.Where("id = ANY(?)", store.Int64s(nil)).Find(&none).Error)
		assert.Empty(t, none, "nil Int64s is the empty array, not NULL")

		res := g.Model(&widget{}).Where("tag = ?", "k").Update("tag", "m")
		require.NoError(t, res.Error)
		assert.EqualValues(t, 2, res.RowsAffected)
		res = g.Model(&widget{}).Where("tag = ?", "absent").Update("tag", "m")
		require.NoError(t, res.Error)
		assert.EqualValues(t, 0, res.RowsAffected)

		// One transaction, not two connections that happen to agree.
		var pgxTx, gormTx int64
		require.NoError(t, tx.QueryRow(ctx, `SELECT txid_current()`).Scan(&pgxTx))
		require.NoError(t, g.Raw(`SELECT txid_current()`).Scan(&gormTx).Error)
		assert.Equal(t, pgxTx, gormTx)

		p, gm := countWidgets(t, ctx, tx)
		assert.Equal(t, [2]int64{3, 3}, [2]int64{p, gm})
	}()

	p, gm := countWidgets(t, ctx, db.Pool)
	assert.Equal(t, [2]int64{0, 0}, [2]int64{p, gm}, "rollback undoes the writes of both sides")
}

func TestGORMRefusals(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Pool)

	assert.ErrorIs(t, g.Transaction(func(*gorm.DB) error { return nil }), store.ErrGORMTx)
	assert.ErrorIs(t, g.Begin().Error, store.ErrGORMTx)
	assert.ErrorIs(t, g.AutoMigrate(&widget{}), store.ErrGORMMigrate)
	assert.ErrorIs(t, g.Migrator().CreateTable(&widget{}), store.ErrGORMMigrate)

	// A statement whose context lost its Querier runs nowhere.
	assert.ErrorIs(t, g.WithContext(ctx).Exec(`SELECT 1`).Error, store.ErrNoQuerier)
	var n int
	assert.ErrorIs(t, g.WithContext(ctx).Raw(`SELECT 1`).Scan(&n).Error, store.ErrNoQuerier)
	assert.ErrorIs(t, store.G(ctx, nil).Exec(`SELECT 1`).Error, store.ErrNoQuerier)
}

func TestGORMErrorsStayPgx(t *testing.T) {
	db, uid := bridgeDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Pool)

	require.NoError(t, g.Create(&widget{Name: "dup"}).Error)
	err := g.Create(&widget{Name: "dup"}).Error
	assert.True(t, store.IsUniqueViolation(err, "widgets_name_key"), "%v", err)
	assert.False(t, store.IsUniqueViolation(err, "other"))
	err = g.Model(&widget{}).Where("name = ?", "dup").Update("name", "dup").Error
	assert.NoError(t, err)

	missing := uid + 1000
	err = g.Create(&widget{Name: "fk", Owner: &missing}).Error
	assert.True(t, store.IsForeignKeyViolation(err), "%v", err)
	err = g.Exec(`UPDATE widgets SET owner = ? WHERE name = 'dup'`, missing).Error
	assert.True(t, store.IsForeignKeyViolation(err), "%v", err)

	var w widget
	err = g.Where("name = ?", "absent").First(&w).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.True(t, store.IsNoRows(err))
	err = g.Where("name = ?", "absent").Take(&w).Error
	assert.True(t, store.IsNoRows(err))
	var id int64
	err = g.Raw(`SELECT id FROM widgets WHERE name = 'absent'`).Row().Scan(&id)
	assert.True(t, store.IsNoRows(err), "%v", err)
	var ws []widget
	assert.NoError(t, g.Where("name = ?", "absent").Find(&ws).Error, "Find of nothing is no error")
	assert.True(t, store.IsNoRows(db.Pool.QueryRow(ctx, `SELECT 1 WHERE false`).Scan(&id)), "pgx path unchanged")
	assert.False(t, store.IsNoRows(errRollback))
}

func TestGORMConcurrentTransactions(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	const workers, perWorker = 16, 5

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			tag := fmt.Sprint("w", i)
			errs[i] = db.Tx(ctx, func(tx pgx.Tx) error {
				g := store.G(ctx, tx)
				for j := range perWorker {
					if err := g.Create(&widget{Name: fmt.Sprint(tag, "-", j), Tag: tag}).Error; err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `INSERT INTO widgets (name, tag) VALUES ($1, $2)`, tag+"-pgx", tag); err != nil {
					return err
				}
				// Only this transaction's rows are visible: the others are
				// uncommitted or committed under other tags.
				var mine, total int64
				if err := g.Model(&widget{}).Where("tag = ?", tag).Count(&mine).Error; err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM widgets WHERE tag = $1`, tag).Scan(&total); err != nil {
					return err
				}
				if mine != perWorker+1 || total != mine {
					return fmt.Errorf("%s sees %d via GORM, %d via pgx", tag, mine, total)
				}
				if i%2 == 1 {
					return errRollback
				}
				return nil
			})
		})
	}
	wg.Wait()

	for i, err := range errs {
		if i%2 == 1 {
			assert.ErrorIs(t, err, errRollback)
		} else {
			assert.NoError(t, err)
		}
	}
	var tags []string
	require.NoError(t, store.G(ctx, db.Pool).Model(&widget{}).Distinct("tag").Order("tag").Pluck("tag", &tags).Error)
	var want []string
	for i := 0; i < workers; i += 2 {
		want = append(want, fmt.Sprint("w", i))
	}
	slices.Sort(want)
	assert.Equal(t, want, tags)
	p, gm := countWidgets(t, ctx, db.Pool)
	assert.Equal(t, [2]int64{workers / 2 * (perWorker + 1), workers / 2 * (perWorker + 1)}, [2]int64{p, gm})
}

func TestGORMContextCancel(t *testing.T) {
	db, _ := bridgeDB(t)
	run := func(name string, f func(ctx context.Context) error) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := f(ctx)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Less(t, time.Since(start), 3*time.Second)
		})
	}
	run("exec", func(ctx context.Context) error {
		return store.G(ctx, db.Pool).Exec(`SELECT pg_sleep(10)`).Error
	})
	run("query", func(ctx context.Context) error {
		var s string
		return store.G(ctx, db.Pool).Raw(`SELECT pg_sleep(10)::text`).Scan(&s).Error
	})
	run("in tx", func(ctx context.Context) error {
		return db.Tx(ctx, func(tx pgx.Tx) error {
			return store.G(ctx, tx).Exec(`SELECT pg_sleep(10)`).Error
		})
	})
}

// TestGORMResultTypes reads each type through the bridge into an any (what
// database/sql receives) and checks the value.
func TestGORMResultTypes(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Pool)
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)

	cases := []struct {
		expr string
		want any
	}{
		{`7::int2`, int64(7)},
		{`-8::int4`, int64(-8)},
		{`9007199254740993::int8`, int64(9007199254740993)},
		{`123456789012.12345678::numeric(20,8)`, "123456789012.12345678"},
		{`1.5::float4`, float64(1.5)},
		{`2.25::float8`, float64(2.25)},
		{`true`, true},
		{`'tê'::text`, "tê"},
		{`'v'::varchar(4)`, "v"},
		{`'ab'::char(3)`, "ab "},
		{`'n'::name`, "n"},
		{`'\x00ff'::bytea`, []byte{0, 0xff}},
		{`'{"a": 1}'::json`, []byte(`{"a": 1}`)},
		{`'{"a":1}'::jsonb`, []byte(`{"a": 1}`)},
		{`'2024-01-02 03:04:05.123456+00'::timestamptz`, ts},
		{`'2024-01-02 03:04:05.123456'::timestamp`, ts},
		{`'2024-01-02'::date`, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
		{`'1 day 02:03:04'::interval`, "1 day 02:03:04"},
		{`'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"},
		{`'10.0.0.1/8'::inet`, "10.0.0.1/8"},
		{`'10.0.0.0/8'::cidr`, "10.0.0.0/8"},
		{`B'1010'::bit(4)`, "1010"},
		{`B'101'::varbit`, "101"},
		{`'{1,2}'::int2[]`, []int64{1, 2}},
		{`'{3}'::int4[]`, []int64{3}},
		{`'{4,5}'::int8[]`, []int64{4, 5}},
		{`'{}'::int8[]`, []int64{}},
		{`'{1.5}'::float4[]`, []float64{1.5}},
		{`'{2.5}'::float8[]`, []float64{2.5}},
		{`'{t,f}'::bool[]`, []bool{true, false}},
		{`'{a,"b,c"}'::text[]`, []string{"a", "b,c"}},
		{`'{v}'::varchar[]`, []string{"v"}},
		{`'{x}'::char(2)[]`, []string{"x "}},
		{`'{1.50}'::numeric[]`, []string{"1.50"}},
		{`'{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}'::uuid[]`, []string{"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}},
		{`ARRAY['{"a":1}'::json]`, [][]byte{[]byte(`{"a":1}`)}},
		{`ARRAY['{"a":1}'::jsonb]`, [][]byte{[]byte(`{"a": 1}`)}},
		{`ARRAY['2024-01-02 03:04:05.123456+00'::timestamptz]`, []time.Time{ts}},
		{`NULL::int8`, nil},
		{`NULL::text`, nil},
		{`NULL::jsonb`, nil},
		{`NULL::int8[]`, nil},
		{`NULL::timestamptz`, nil},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			var got any
			require.NoError(t, g.Raw(`SELECT `+c.expr).Row().Scan(&got))
			if want, ok := c.want.(time.Time); ok {
				require.IsType(t, want, got)
				assert.True(t, want.Equal(got.(time.Time)), "%v != %v", got, want)
				return
			}
			if want, ok := c.want.([]time.Time); ok {
				require.IsType(t, want, got)
				require.Len(t, got, 1)
				assert.True(t, want[0].Equal(got.([]time.Time)[0]))
				return
			}
			assert.Equal(t, c.want, got)
		})
	}

	t.Run("typed destinations", func(t *testing.T) {
		var r struct {
			Small  int16
			Count  int
			Cost   float64
			CostS  string
			Flag   bool
			Raw    []byte
			Doc    string
			At     time.Time
			Opt    *time.Time
			Refs   store.Int64s
			Tags   store.Strings
			NoTags store.Strings
		}
		require.NoError(t, g.Raw(`SELECT 3::int2 AS small, 4::int4 AS count, 1.25::numeric AS cost, 1.25::numeric(10,4) AS cost_s,
			true AS flag, '\x01'::bytea AS raw, '{"k":"v"}'::jsonb AS doc, now() AS at, NULL::timestamptz AS opt,
			'{1,2}'::int8[] AS refs, '{a}'::varchar[] AS tags, NULL::text[] AS no_tags`).Scan(&r).Error)
		assert.Equal(t, int16(3), r.Small)
		assert.Equal(t, 4, r.Count)
		assert.Equal(t, 1.25, r.Cost)
		assert.Equal(t, "1.2500", r.CostS)
		assert.True(t, r.Flag)
		assert.Equal(t, []byte{1}, r.Raw)
		assert.Equal(t, `{"k": "v"}`, r.Doc)
		assert.WithinDuration(t, time.Now(), r.At, time.Minute)
		assert.Nil(t, r.Opt)
		assert.Equal(t, store.Int64s{1, 2}, r.Refs)
		assert.Equal(t, store.Strings{"a"}, r.Tags)
		assert.Nil(t, r.NoTags)
	})

	t.Run("refused", func(t *testing.T) {
		for _, expr := range []string{`point(1, 2)`, `'1'::money`, `'a'::tsvector`, `ROW(1, 2)`, `'{"[1,2)"}'::int4range[]`} {
			var got any
			err := g.Raw(`SELECT ` + expr + ` AS v`).Row().Scan(&got)
			require.Error(t, err, expr)
			assert.Contains(t, err.Error(), `column "v" has type`, expr)
			assert.Contains(t, err.Error(), "does not read", expr)
		}
		var got any
		err := g.Raw(`SELECT ARRAY[1, NULL]::int8[] AS v`).Row().Scan(&got)
		assert.ErrorContains(t, err, "NULL", "a NULL element has no slice form")
	})
}

// TestGORMReadsEverySchemaColumn selects every column of every table the
// migrations create through the bridge, so a migration that brings a type
// the bridge does not read fails here rather than in production.
func TestGORMReadsEverySchemaColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(t)
	// River's own enum; River reads that table, the services never do.
	unread := map[string]bool{"river_job.state": true}

	rows, err := db.Pool.Query(ctx, `SELECT c.table_name, c.column_name, c.udt_name
		FROM information_schema.columns c
		JOIN information_schema.tables t USING (table_schema, table_name)
		WHERE c.table_schema = 'public' AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_name, c.ordinal_position`)
	require.NoError(t, err)
	cols := map[string][]string{}
	udts := map[string]bool{}
	for rows.Next() {
		var table, col, udt string
		require.NoError(t, rows.Scan(&table, &col, &udt))
		udts[udt] = true
		if !unread[table+"."+col] {
			cols[table] = append(cols[table], pgx.Identifier{col}.Sanitize())
		}
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, cols)
	t.Logf("column types: %v", slices.Sorted(func(yield func(string) bool) {
		for u := range udts {
			if !yield(u) {
				return
			}
		}
	}))

	g := store.G(ctx, db.Pool)
	for table, list := range cols {
		q := `SELECT ` + strings.Join(list, ", ") + ` FROM ` + pgx.Identifier{table}.Sanitize() + ` LIMIT 0`
		var out []map[string]any
		assert.NoError(t, g.Raw(q).Scan(&out).Error, table)
	}
	var out []map[string]any
	err = g.Raw(`SELECT state FROM river_job LIMIT 0`).Scan(&out).Error
	assert.ErrorContains(t, err, "does not read", "an enum is refused, even with no rows")
	assert.NoError(t, g.Raw(`SELECT state::text FROM river_job LIMIT 0`).Scan(&out).Error)
}

// TestArraysThroughPgx checks that Int64s and Strings still read through
// pgx itself, in both wire formats, now that they implement sql.Scanner.
func TestArraysThroughPgx(t *testing.T) {
	db, _ := bridgeDB(t)
	ctx := context.Background()
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		var ids store.Int64s
		var tags, none store.Strings
		require.NoError(t, db.Pool.QueryRow(ctx, `SELECT '{1,2}'::int8[], '{a,"b c"}'::text[], NULL::text[]`, mode).Scan(&ids, &tags, &none))
		assert.Equal(t, store.Int64s{1, 2}, ids, mode)
		assert.Equal(t, store.Strings{"a", "b c"}, tags, mode)
		assert.Nil(t, none, mode)
		var empty store.Int64s
		require.NoError(t, db.Pool.QueryRow(ctx, `SELECT $1::int8[]`, mode, store.Int64s(nil)).Scan(&empty))
		assert.Equal(t, store.Int64s{}, empty, mode)
	}
}

// account is a hand-written model of a real table with text[] columns and
// no gorm type tags: Int64s and Strings map as columns by themselves.
type account struct {
	ID                int64
	Name              string
	PluginKey         string
	Type              string
	CredentialsEnc    []byte
	Models            store.Strings
	HeaderPassthrough store.Strings
	CreatedBy         *int64
}

func (account) TableName() string { return "accounts" }

func TestGORMArrayColumnsOnARealTable(t *testing.T) {
	db, uid := bridgeDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Pool)

	a := account{Name: "arr", PluginKey: "p", Type: "t", CredentialsEnc: []byte{1}, Models: store.Strings{"m1", "m 2"}, CreatedBy: &uid}
	require.NoError(t, g.Create(&a).Error)
	require.NotZero(t, a.ID)

	var back account
	require.NoError(t, g.Take(&back, a.ID).Error)
	assert.Equal(t, store.Strings{"m1", "m 2"}, back.Models)
	assert.Equal(t, store.Strings{}, back.HeaderPassthrough, "nil wrote the empty array, NOT NULL holds")

	res := g.Model(&account{}).Where("id = ?", a.ID).Update("header_passthrough", store.Strings{"x-a"})
	require.NoError(t, res.Error)
	assert.EqualValues(t, 1, res.RowsAffected)
	var hp []string
	require.NoError(t, db.Pool.QueryRow(ctx, `SELECT header_passthrough FROM accounts WHERE id = $1`, a.ID).Scan(&hp))
	assert.Equal(t, []string{"x-a"}, hp)

	// Row().Scan: GORM's Scan reads a slice destination as one row per
	// element.
	var ids store.Int64s
	require.NoError(t, g.Raw(`SELECT array_agg(id ORDER BY id) FROM accounts WHERE models && ?`, store.Strings{"m 2"}).Row().Scan(&ids))
	assert.Equal(t, store.Int64s{a.ID}, ids)
}

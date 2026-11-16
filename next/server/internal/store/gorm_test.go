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

// widgetDB is a migrated database with a widgets table, plus one user id to
// refer to.
func widgetDB(t testing.TB) (*store.DB, int64) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(t)
	_, err := db.SQL.ExecContext(ctx, `CREATE TABLE widgets (
		id bigserial PRIMARY KEY,
		name text NOT NULL UNIQUE,
		owner bigint REFERENCES users(id),
		tag text NOT NULL DEFAULT '',
		tags text[] NOT NULL DEFAULT '{}',
		refs bigint[] NOT NULL DEFAULT '{}')`)
	require.NoError(t, err)
	var uid int64
	require.NoError(t, db.SQL.QueryRowContext(ctx, `INSERT INTO users (email, password_hash) VALUES ('widget@test', 'x') RETURNING id`).Scan(&uid))
	return db, uid
}

var errRollback = errors.New("rollback on purpose")

// countWidgets counts the widgets q sees, through a model and through raw
// SQL.
func countWidgets(t *testing.T, ctx context.Context, q *gorm.DB) (viaRaw, viaModel int64) {
	t.Helper()
	require.NoError(t, store.G(ctx, q).Raw(`SELECT count(*) FROM widgets`).Row().Scan(&viaRaw))
	require.NoError(t, store.G(ctx, q).Model(&widget{}).Count(&viaModel).Error)
	return viaRaw, viaModel
}

// GORM statements and database/sql statements on the transaction's
// *sql.Tx (SQLTx, as River's InsertTx uses it) share one transaction.
func TestGORMAndSQLTxShareTheTransaction(t *testing.T) {
	db, uid := widgetDB(t)
	ctx := context.Background()

	// A transaction rolled back by defer, unlike DB.Tx, also when a require
	// below ends the test with runtime.Goexit.
	tx, err := testutil.Begin(ctx, db)
	require.NoError(t, err)
	func() {
		defer tx.Rollback() //nolint:errcheck
		g := store.G(ctx, tx)
		w := widget{Name: "a", Owner: &uid, Tags: store.Strings{"x", "y"}, Refs: store.Int64s{3, 4}}
		require.NoError(t, g.Create(&w).Error)
		require.NotZero(t, w.ID, "RETURNING id scanned back")

		// GORM -> database/sql.
		var name string
		require.NoError(t, store.SQLTx(tx).QueryRowContext(ctx, `SELECT name FROM widgets WHERE id = $1`, w.ID).Scan(&name))
		assert.Equal(t, "a", name)

		// database/sql -> GORM.
		_, err = store.SQLTx(tx).ExecContext(ctx, `INSERT INTO widgets (name, tag) VALUES ('b', 'k'), ('c', 'k')`)
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
		var sqlTx, gormTx int64
		require.NoError(t, store.SQLTx(tx).QueryRowContext(ctx, `SELECT txid_current()`).Scan(&sqlTx))
		require.NoError(t, g.Raw(`SELECT txid_current()`).Scan(&gormTx).Error)
		assert.Equal(t, sqlTx, gormTx)

		r, m := countWidgets(t, ctx, tx)
		assert.Equal(t, [2]int64{3, 3}, [2]int64{r, m})
	}()

	r, m := countWidgets(t, ctx, db.Gorm)
	assert.Equal(t, [2]int64{0, 0}, [2]int64{r, m}, "rollback undoes the writes of both sides")
	assert.Nil(t, store.SQLTx(db.Gorm), "the pool's handle is no transaction")
}

func TestGORMErrors(t *testing.T) {
	db, uid := widgetDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Gorm)

	require.NoError(t, g.Create(&widget{Name: "dup"}).Error)
	err := g.Create(&widget{Name: "dup"}).Error
	assert.True(t, store.IsUniqueViolation(err, "widgets_name_key"), "%v", err)
	assert.False(t, store.IsUniqueViolation(err, "other"))
	assert.Equal(t, "23505", store.SQLState(err))
	assert.Equal(t, "widgets_name_key", store.ConstraintName(err))
	err = g.Model(&widget{}).Where("name = ?", "dup").Update("name", "dup").Error
	assert.NoError(t, err)

	missing := uid + 1000
	err = g.Create(&widget{Name: "fk", Owner: &missing}).Error
	assert.True(t, store.IsForeignKeyViolation(err), "%v", err)
	err = g.Exec(`UPDATE widgets SET owner = ? WHERE name = 'dup'`, missing).Error
	assert.True(t, store.IsForeignKeyViolation(err), "%v", err)
	assert.Empty(t, store.SQLState(errRollback))
	assert.Empty(t, store.ConstraintName(nil))

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
	assert.True(t, store.IsNoRows(db.SQL.QueryRowContext(ctx, `SELECT 1 WHERE false`).Scan(&id)), "database/sql's own")
	assert.False(t, store.IsNoRows(errRollback))
}

func TestGORMConcurrentTransactions(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	const workers, perWorker = 16, 5

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			tag := fmt.Sprint("w", i)
			errs[i] = db.Tx(ctx, func(tx *gorm.DB) error {
				g := store.G(ctx, tx)
				for j := range perWorker {
					if err := g.Create(&widget{Name: fmt.Sprint(tag, "-", j), Tag: tag}).Error; err != nil {
						return err
					}
				}
				if err := g.Exec(`INSERT INTO widgets (name, tag) VALUES (?, ?)`, tag+"-raw", tag).Error; err != nil {
					return err
				}
				// Only this transaction's rows are visible: the others are
				// uncommitted or committed under other tags.
				var mine, total int64
				if err := g.Model(&widget{}).Where("tag = ?", tag).Count(&mine).Error; err != nil {
					return err
				}
				if err := store.SQLTx(tx).QueryRowContext(ctx, `SELECT count(*) FROM widgets WHERE tag = $1`, tag).Scan(&total); err != nil {
					return err
				}
				if mine != perWorker+1 || total != mine {
					return fmt.Errorf("%s sees %d via GORM, %d via database/sql", tag, mine, total)
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
	require.NoError(t, store.G(ctx, db.Gorm).Model(&widget{}).Distinct("tag").Order("tag").Pluck("tag", &tags).Error)
	var want []string
	for i := 0; i < workers; i += 2 {
		want = append(want, fmt.Sprint("w", i))
	}
	slices.Sort(want)
	assert.Equal(t, want, tags)
	r, m := countWidgets(t, ctx, db.Gorm)
	assert.Equal(t, [2]int64{workers / 2 * (perWorker + 1), workers / 2 * (perWorker + 1)}, [2]int64{r, m})
}

func TestGORMContextCancel(t *testing.T) {
	db, _ := widgetDB(t)
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
		return store.G(ctx, db.Gorm).Exec(`SELECT pg_sleep(10)`).Error
	})
	run("query", func(ctx context.Context) error {
		var s string
		return store.G(ctx, db.Gorm).Raw(`SELECT pg_sleep(10)::text`).Scan(&s).Error
	})
	run("in tx", func(ctx context.Context) error {
		return db.Tx(ctx, func(tx *gorm.DB) error {
			return store.G(ctx, tx).Exec(`SELECT pg_sleep(10)`).Error
		})
	})
}

// TestGORMResultTypes reads each type into an any, which is what pgx's
// database/sql driver hands over and what a store type's Scan receives.
func TestGORMResultTypes(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Gorm)
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
		{`'infinity'::timestamptz`, "infinity"},
		{`'-infinity'::timestamptz`, "-infinity"},
		{`'1 day 02:03:04'::interval`, "1 day 02:03:04"},
		{`'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"},
		{`'10.0.0.1/8'::inet`, "10.0.0.1/8"},
		{`B'1010'::bit(4)`, "1010"},
		{`'pending'::text::river_job_state`, "pending"},
		// Arrays come as their text, which Int64s, Strings and Times parse.
		{`'{1,2}'::int2[]`, "{1,2}"},
		{`'{}'::int8[]`, "{}"},
		{`'{a,"b,c",NULL}'::text[]`, `{a,"b,c",NULL}`},
		{`ARRAY['2024-01-02 03:04:05.123456+00'::timestamptz]`, `{"2024-01-02 03:04:05.123456+00"}`},
		{`NULL::int8`, nil},
		{`NULL::text`, nil},
		{`NULL::jsonb`, nil},
		{`NULL::int8[]`, nil},
		{`NULL::timestamptz`, nil},
	}
	// The session's zone decides how timestamps print as text.
	require.NoError(t, db.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		if err := g.Exec(`SET LOCAL TimeZone = 'UTC'`).Error; err != nil {
			return err
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
				assert.Equal(t, c.want, got)
			})
		}
		return nil
	}))

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
			Times  store.Times
		}
		require.NoError(t, g.Raw(`SELECT 3::int2 AS small, 4::int4 AS count, 1.25::numeric AS cost, 1.25::numeric(10,4) AS cost_s,
			true AS flag, '\x01'::bytea AS raw, '{"k":"v"}'::jsonb AS doc, now() AS at, NULL::timestamptz AS opt,
			'{1,2}'::int8[] AS refs, '{a}'::varchar[] AS tags, NULL::text[] AS no_tags,
			ARRAY[?::timestamptz] AS times`, ts).Scan(&r).Error)
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
		require.Len(t, r.Times, 1)
		assert.True(t, ts.Equal(r.Times[0]))
	})

	t.Run("NULL elements", func(t *testing.T) {
		var ids store.Int64s
		err := g.Raw(`SELECT ARRAY[1, NULL]::int8[]`).Row().Scan(&ids)
		assert.ErrorContains(t, err, "NULL", "a NULL element has no slice form")
	})
}

// TestGORMReadsEverySchemaColumn selects every column of every table the
// migrations create, so a migration that brings a type the driver cannot
// hand over fails here rather than in production.
func TestGORMReadsEverySchemaColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(t)

	rows, err := db.SQL.QueryContext(ctx, `SELECT c.table_name, c.column_name, c.udt_name
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
		cols[table] = append(cols[table], store.QuoteIdent(col))
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

	g := store.G(ctx, db.Gorm)
	for table, list := range cols {
		q := `SELECT ` + strings.Join(list, ", ") + ` FROM ` + store.QuoteIdent(table) + ` LIMIT 0`
		var out []map[string]any
		assert.NoError(t, g.Raw(q).Scan(&out).Error, table)
	}
}

// Array parameters: plain slices for pgx to encode, in both of pgx's ways of
// sending a statement.
func TestStoreTypesAsParameters(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	at := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)
	simple, err := store.OpenSized(ctx, testutil.DSN(db)+"&default_query_exec_mode=simple_protocol", 1)
	require.NoError(t, err)
	defer simple.Close()
	for name, pool := range map[string]*store.DB{"extended": db, "simple": simple} {
		t.Run(name, func(t *testing.T) {
			g := store.G(ctx, pool.Gorm)
			var r struct {
				Ids   store.Int64s
				Tags  store.Strings
				Flags string
				Times store.Times
			}
			require.NoError(t, g.Raw(`SELECT ?::int8[] AS ids, ?::text[] AS tags, ?::bool[]::text AS flags, ?::timestamptz[] AS times`,
				store.Int64s{-1, 0, 9007199254740993}, store.Strings{"", "a b", `q"uote`, `back\slash`, "NULL", "{,}"},
				store.Bools{true, false}, store.Times{at}).Scan(&r).Error)
			assert.Equal(t, store.Int64s{-1, 0, 9007199254740993}, r.Ids)
			assert.Equal(t, store.Strings{"", "a b", `q"uote`, `back\slash`, "NULL", "{,}"}, r.Tags)
			assert.Equal(t, "{t,f}", r.Flags)
			require.Len(t, r.Times, 1)
			assert.True(t, at.Truncate(time.Microsecond).Equal(r.Times[0]), "%v", r.Times[0])

			var n int64
			require.NoError(t, g.Raw(`SELECT count(*) FROM unnest(?::int8[]) x WHERE x = ANY(?)`,
				store.Int64s(nil), store.Int64s{1}).Row().Scan(&n))
			assert.Zero(t, n, "nil is the empty array")
		})
	}
}

// The array types hand pgx their plain slice as the value (see
// store.Int64s), which database/sql itself would refuse: this keeps that
// pgx's driver takes it and encodes it as the parameter's array type, with
// or without a cast, and that GORM binds each as one parameter rather than
// expanding it into a list.
func TestArrayParametersRelyOnPgxStdlib(t *testing.T) {
	db, _ := widgetDB(t)
	ctx := context.Background()
	at := time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC)
	one := int64(1)
	params := []any{
		store.Int64s{3, 1}, store.Strings{"a", `"b"`}, store.Times{at}, store.Bools{true, false},
		store.NullStrings([]string{"x"}), store.Array[*int64]{&one, nil}, store.Int64s(nil),
	}

	// GORM: one placeholder each, the value itself in the statement's Vars.
	q := store.DryRun().Exec(`SELECT ?::int8[], ?::text[], ?::timestamptz[], ?::bool[], ?::text[], ?::int8[], ?::int8[]`, params...)
	require.NoError(t, q.Error)
	require.Equal(t, `SELECT $1::int8[], $2::text[], $3::timestamptz[], $4::bool[], $5::text[], $6::int8[], $7::int8[]`, q.Statement.SQL.String())
	require.Equal(t, params, q.Statement.Vars)

	// pgx: the same values come back, through database/sql's text form.
	var r struct {
		Ids   store.Int64s
		Tags  store.Strings
		Times store.Times
		Flags string
		Only  store.Strings
		Ptrs  string
		Empty store.Int64s
	}
	g := store.G(ctx, db.Gorm)
	require.NoError(t, g.Raw(`SELECT ?::int8[] AS ids, ?::text[] AS tags, ?::timestamptz[] AS times, ?::bool[]::text AS flags,
		?::text[] AS only, ?::int8[]::text AS ptrs, ?::int8[] AS empty`, params...).Scan(&r).Error)
	assert.Equal(t, store.Int64s{3, 1}, r.Ids)
	assert.Equal(t, store.Strings{"a", `"b"`}, r.Tags)
	require.Len(t, r.Times, 1)
	assert.True(t, at.Truncate(time.Microsecond).Equal(r.Times[0]), "%v", r.Times[0])
	assert.Equal(t, "{t,f}", r.Flags)
	assert.Equal(t, store.Strings{"x"}, r.Only)
	assert.Equal(t, "{1,NULL}", r.Ptrs)
	assert.Equal(t, store.Int64s{}, r.Empty, "nil is the empty array, not NULL")

	// Without a cast the parameter takes its type from its use.
	var n int64
	require.NoError(t, g.Raw(`SELECT count(*) FROM generate_series(1, 5) x WHERE x = ANY(?)`, store.Int64s{2, 4, 9}).Row().Scan(&n))
	assert.EqualValues(t, 2, n)
	require.NoError(t, g.Raw(`SELECT count(*) FROM (VALUES ('a'), ('b'), ('c')) v(s) WHERE s = ANY(?)`, store.Strings{"c", "a"}).Row().Scan(&n))
	assert.EqualValues(t, 2, n)
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
	db, uid := widgetDB(t)
	ctx := context.Background()
	g := store.G(ctx, db.Gorm)

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
	var hp store.Strings
	require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT header_passthrough FROM accounts WHERE id = $1`, a.ID).Scan(&hp))
	assert.Equal(t, store.Strings{"x-a"}, hp)

	// Row().Scan: GORM's Scan reads a slice destination as one row per
	// element.
	var ids store.Int64s
	require.NoError(t, g.Raw(`SELECT array_agg(id ORDER BY id) FROM accounts WHERE models && ?`, store.Strings{"m 2"}).Row().Scan(&ids))
	assert.Equal(t, store.Int64s{a.ID}, ids)
}

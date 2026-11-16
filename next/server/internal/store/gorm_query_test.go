package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// A row lock is a value: refining or changing the one a caller got leaves
// every other caller's lock as it was (G3 of the GORM review: ForUpdate was
// a package variable, one assignment away from SKIP LOCKED everywhere).
func TestRowLockIsAValue(t *testing.T) {
	mine := store.ForUpdate()
	mine.Options = clause.LockingOptionsSkipLocked
	_ = mine.Of("a").SkipLocked()
	assert.Equal(t, clause.Locking{Strength: clause.LockingStrengthUpdate}, store.ForUpdate().Locking)
	base := store.Lock(store.LockingStrengthKeyShare)
	_ = base.Of("g").SkipLocked()
	assert.Equal(t, clause.Locking{Strength: store.LockingStrengthKeyShare}, base.Locking)
	for lock, want := range map[store.RowLock]string{
		store.ForUpdate():                               "FOR UPDATE",
		store.ForUpdate().Of("a").SkipLocked():          `FOR UPDATE OF "a" SKIP LOCKED`,
		store.Lock(store.LockingStrengthNoKeyUpdate):    "FOR NO KEY UPDATE",
		store.Lock(clause.LockingStrengthShare).Of("g"): `FOR SHARE OF "g"`,
	} {
		sql, _, err := gormtest.Build(func(db *gorm.DB) *gorm.DB {
			return db.Table("t").Select("id").Clauses(lock).Take(&numberedSink)
		})
		require.NoError(t, err)
		assert.Equal(t, `SELECT id FROM "t" LIMIT $1 `+want, sql)
	}
}

// Conds ANDs its conditions in the order added, a scope included; none
// leaves the statement without a WHERE.
func TestSnapshotConds(t *testing.T) {
	var f store.Conds
	f.And("name ILIKE ?", "%a%")
	f = append(f, func(db *gorm.DB) *gorm.DB { return db.Where("owner = ?", 7) })
	f.And("refs && ?", store.Int64s{1, 2})
	t.Run("some", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return db.Table("widgets").Scopes(f.Scope()).Find(&[]widget{}) })
	})
	t.Run("none", func(t *testing.T) {
		gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
			return db.Table("widgets").Scopes(store.Conds(nil).Scope()).Find(&[]widget{})
		})
	})
}

func TestSetIf(t *testing.T) {
	name, zero := "a", 0
	set := map[string]any{}
	store.SetIf(set, "name", &name)
	store.SetIf(set, "port", &zero)
	store.SetIf[string](set, "host", nil)
	assert.Equal(t, map[string]any{"name": "a", "port": 0}, set)
}

func TestNullStrings(t *testing.T) {
	assert.Nil(t, store.NullStrings(nil))
	assert.Equal(t, store.Strings{}, store.NullStrings([]string{}))
	assert.Equal(t, store.Strings{"a"}, store.NullStrings([]string{"a"}))
}

// pair is a row of TestCollectRows.
type pair struct{ n, double int64 }

func scanPair(rows *sql.Rows) (pair, error) {
	var p pair
	err := rows.Scan(&p.n, &p.double)
	return p, err
}

// CollectRows reads every row in order and is never nil; EachRow folds them
// as they come. An error of the query or of a scan stops the read.
func TestCollectRows(t *testing.T) {
	db, _ := widgetDB(t)
	g := store.G(context.Background(), db.Gorm)
	series := func(n int) *gorm.DB { return g.Raw(`SELECT n, n * 2 FROM generate_series(1, ?::int) n ORDER BY n`, n) }

	got, err := store.CollectRows(series(3), scanPair)
	require.NoError(t, err)
	assert.Equal(t, []pair{{1, 2}, {2, 4}, {3, 6}}, got)

	none, err := store.CollectRows(series(0), scanPair)
	require.NoError(t, err)
	assert.NotNil(t, none, "no rows is the empty list, not nil")
	assert.Empty(t, none)

	var sum int64
	require.NoError(t, store.EachRow(series(4), func(rows *sql.Rows) error {
		p, err := scanPair(rows)
		sum += p.double
		return err
	}))
	assert.EqualValues(t, 20, sum)

	stop := errors.New("stop")
	calls := 0
	err = store.EachRow(series(3), func(*sql.Rows) error { calls++; return stop })
	assert.ErrorIs(t, err, stop)
	assert.Equal(t, 1, calls, "the first error ends the read")

	bad, err := store.CollectRows(series(2), func(rows *sql.Rows) (string, error) {
		var a, b, c string
		err := rows.Scan(&a, &b, &c) // one column too many
		return a, err
	})
	assert.Error(t, err)
	assert.Nil(t, bad)

	_, err = store.CollectRows(g.Raw(`SELECT no_such_column FROM widgets`), scanPair)
	assert.ErrorContains(t, err, "no_such_column")
}

// Bools(nil) is the empty boolean[], not NULL, on a real server.
func TestBoolsOnTheServer(t *testing.T) {
	db, _ := widgetDB(t)
	g := store.G(context.Background(), db.Gorm)
	var n *int
	require.NoError(t, g.Raw(`SELECT cardinality(?::boolean[])`, store.Bools(nil)).Row().Scan(&n))
	require.NotNil(t, n, "nil is the empty array, not NULL")
	assert.Equal(t, 0, *n)
	var flags []bool
	require.NoError(t, g.Raw(`SELECT ?::boolean[]`, store.Bools{true, false}).Row().Scan(&flags))
	assert.Equal(t, []bool{true, false}, flags)
}

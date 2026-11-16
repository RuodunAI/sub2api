package store_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// numberedSink stands for the row a statement reads, which a snapshot does
// not see.
var numberedSink struct{}

func TestNumberedRaw(t *testing.T) {
	sql, vars, err := gormtest.Build(func(db *gorm.DB) *gorm.DB {
		return store.NewNumbered(`SELECT '$1', $2, E'\\' FROM t WHERE a = $1 AND (j ? 'k' OR b = $2) OR c = $10 OR d = '?'`).Raw(db,
			"a", 2, 3, 4, 5, 6, 7, 8, 9, "ten").Scan(&numberedSink)
	})
	require.NoError(t, err)
	require.Equal(t, `SELECT '$1', $1, E'\\' FROM t WHERE a = $2 AND (j ? 'k' OR b = $3) OR c = $4 OR d = '?'`, sql)
	require.Equal(t, []any{2, "a", 2, "ten"}, vars)
	for _, bad := range []string{`a = $3`, `a = $0`, `a = $`, `a = '@x' AND b = $1`} {
		require.Panics(t, func() { store.NewNumbered(bad).Raw(gormtest.DB(), 1, 2) }, bad)
	}
}

// A statement rewritten once binds each argument wherever the text names
// it, in the text's order, and a ? of the text to itself.
func TestNumberedStatement(t *testing.T) {
	st := store.NewNumbered(`SELECT $2::int, $1::text, $2::int, j ? 'k', $10`)
	require.Equal(t, `SELECT ?::int, ?::text, ?::int, j ? 'k', ?`, st.SQL())
	args := []any{"a", 2, 3, 4, 5, 6, 7, 8, 9, "ten"}
	require.Equal(t, []any{2, "a", 2, clause.Expr{SQL: "?"}, "ten"}, st.Vars(args...))
	// The arguments are checked when bound: a statement is rewritten
	// before any call.
	require.Panics(t, func() { st.Vars("a", 2) })
	for _, bad := range []string{`SELECT @x`, `SELECT $0`, `SELECT $`} {
		require.Panics(t, func() { store.NewNumbered(bad) }, bad)
	}
}

// With names, $name binds the argument at the name's position (numbers
// still bind theirs), every name must be used, and an unknown name, or a
// name without names, is malformed.
func TestNumberedNames(t *testing.T) {
	st := store.NewNumbered(`SELECT $user::bigint, $key, $user, '$user', $2, $refund_key`, "user", "key", "refund_key")
	require.Equal(t, `SELECT ?::bigint, ?, ?, '$user', ?, ?`, st.SQL())
	require.Equal(t, []any{7, "k", 7, "k", "r"}, st.Vars(7, "k", "r"))
	require.Panics(t, func() { store.NewNumbered(`SELECT $user`, "user", "unused") })
	require.Panics(t, func() { store.NewNumbered(`SELECT $other`, "user") })
	require.Panics(t, func() { store.NewNumbered(`SELECT $user`) })
}

// A $ inside a literal stays text however the literal escapes its quote:
// a doubled quote in either kind, \' in E'...' only (in '...' a backslash
// is a byte).
func TestNumberedQuotes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT 'it''s $1', $1`, `SELECT 'it''s $1', ?`},
		{`SELECT E'it\'s $1', $1`, `SELECT E'it\'s $1', ?`},
		{`SELECT e'it''s \' $1', $1`, `SELECT e'it''s \' $1', ?`},
		{`SELECT E'a\\', $1`, `SELECT E'a\\', ?`},
		{`SELECT E'a\\''$1', $1`, `SELECT E'a\\''$1', ?`},
		{`SELECT 'a\', $1`, `SELECT 'a\', ?`},
		{`SELECT TYPE'a\', $1`, `SELECT TYPE'a\', ?`},
		{`SELECT E'\?', $1`, `SELECT E'\?', ?`},
		{`SELECT ''$1`, `SELECT ''?`},
	} {
		st := store.NewNumbered(c.in)
		require.Equal(t, c.want, st.SQL(), c.in)
		vars := st.Vars("v")
		require.Equal(t, "v", vars[len(vars)-1], c.in)
	}
}

// A []byte or json.RawMessage argument stays one parameter right after "(",
// where GORM spreads a slice into one placeholder per element and turns an
// empty one into NULL.
func TestNumberedBytes(t *testing.T) {
	sql, vars, err := gormtest.Build(func(db *gorm.DB) *gorm.DB {
		return store.NewNumbered(`UPDATE t SET a = COALESCE($1, a), b = COALESCE($2, b), c = ($3::jsonb), d = $4 WHERE id = $5`).Exec(db,
			[]byte("ab"), []byte{}, json.RawMessage(`{"k":1}`), json.RawMessage{}, 7)
	})
	require.NoError(t, err)
	require.Equal(t, `UPDATE t SET a = COALESCE($1, a), b = COALESCE($2, b), c = ($3::jsonb), d = $4 WHERE id = $5`, sql)
	require.Equal(t, []any{store.Bytes("ab"), store.Bytes{}, store.Bytes(`{"k":1}`), store.Bytes{}, 7}, vars)
	empty, err := vars[1].(store.Bytes).Value()
	require.NoError(t, err)
	require.Equal(t, []byte{}, empty, "an empty slice is the empty value, not NULL")
}

// What is no SQL code - a comment, a quoted identifier, a dollar-quoted
// string - keeps its $ as text, whatever quotes it holds: an apostrophe in
// a comment does not open a literal that would hide the placeholders after
// it (G2 of the GORM review: "-- the user's id" left $1 unbound and both
// placeholders took the second argument).
func TestNumberedSkipsNonCode(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"SELECT * FROM t WHERE a = $2 -- the user's id\n AND b = $1", "SELECT * FROM t WHERE a = ? -- the user's id\n AND b = ?"},
		{"SELECT $2, $1 -- trailing $1 '", "SELECT ?, ? -- trailing $1 '"},
		{`SELECT $2 /* it's /* nested $1 */ still */, $1`, `SELECT ? /* it's /* nested $1 */ still */, ?`},
		{`SELECT "it's $1", $2, $1`, `SELECT "it's $1", ?, ?`},
		{`SELECT "a""b'$1", $2, $1`, `SELECT "a""b'$1", ?, ?`},
		{`SELECT $$it's $1$$, $2, $1`, `SELECT $$it's $1$$, ?, ?`},
		{`SELECT $fn$ $1 $x$ $fn$, $2, $1`, `SELECT $fn$ $1 $x$ $fn$, ?, ?`},
		{`SELECT a-$2, $1`, `SELECT a-?, ?`},
	} {
		st := store.NewNumbered(c.in)
		require.Equal(t, c.want, st.SQL(), c.in)
		require.Equal(t, []any{"B", "A"}, st.Vars("A", "B"), c.in)
	}
	// A ? there is still text GORM would bind: it is bound to itself.
	st := store.NewNumbered("SELECT $1 -- why?\n, \"q?\", $$?$$")
	require.Equal(t, []any{"A", clause.Expr{SQL: "?"}, clause.Expr{SQL: "?"}, clause.Expr{SQL: "?"}}, st.Vars("A"))
}

// A literal, identifier, dollar-quoted string or block comment left open
// would hide every placeholder after it: it panics like any malformed SQL.
func TestNumberedUnterminated(t *testing.T) {
	for _, bad := range []string{
		`SELECT 'open, $1`,
		`SELECT E'open\', $1`,
		`SELECT "open, $1`,
		`SELECT $$open, $1`,
		`SELECT $tag$open$ta, $1`,
		`SELECT /* open /* nested */, $1`,
	} {
		require.Panics(t, func() { store.NewNumbered(bad) }, bad)
	}
}

package core_test

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestScopeTableScopeSQL(t *testing.T) {
	s := core.DataScope{Owner: 7, Groups: []int64{0, 3}, IDs: []int64{5}, PairG: []int64{2}, PairID: []int64{9}}
	// The predicate text is fixed per table: one plan whatever the ids.
	for _, c := range []struct {
		t                core.ScopeTable
		table, id, where string
	}{{core.AccountRows, "accounts a", "a.id", "($1::boolean OR COALESCE(a.created_by = $2::bigint, false) OR a.id = ANY($3::bigint[])" +
		" OR EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ($4::boolean OR ag.group_id = ANY($5::bigint[])))" +
		" OR EXISTS (SELECT 1 FROM account_groups ag JOIN unnest($6::bigint[], $7::bigint[]) sp(g, x) ON sp.x = ag.account_id AND (sp.g = 0 OR sp.g = ag.group_id) WHERE ag.account_id = a.id))"},
		{core.ProxyRows, "proxies p", "p.id", "($1::boolean OR COALESCE(p.created_by = $2::bigint, false) OR p.id = ANY($3::bigint[]))"}} {
		query, vars, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB {
			var ids []int64
			return tx.Table(c.table).Select(c.id).Scopes(c.t.Scope(s)).Find(&ids)
		})
		require.NoError(t, err)
		assert.Equal(t, "SELECT "+c.id+" FROM "+c.table+" WHERE "+c.where, query)
		want := c.t.Args(s)
		require.Len(t, vars, len(want))
		for i, v := range vars {
			if ids, ok := want[i].([]int64); ok {
				valuer, isValuer := v.(driver.Valuer)
				require.True(t, isValuer, "arg %d is one array parameter, not %T", i, v)
				got, err := valuer.Value()
				require.NoError(t, err)
				assert.Equal(t, ids, got, "arg %d", i)
			} else {
				assert.Equal(t, want[i], v, "arg %d", i)
			}
		}
	}
}

// TestScopeTableScopeMatchesAllowsRow reads one data set through the GORM
// scope and checks it against DataScope.AllowsRow.
func TestScopeTableScopeMatchesAllowsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping database test in short mode")
	}
	ctx := context.Background()
	db := testutil.DB(t)
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var id int64
		require.NoError(t, db.SQL.QueryRowContext(ctx, sql+" RETURNING id", args...).Scan(&id))
		return id
	}
	u1 := one(`INSERT INTO users (email, password_hash) VALUES ('u1@test', 'x')`)
	u2 := one(`INSERT INTO users (email, password_hash) VALUES ('u2@test', 'x')`)
	g1 := one(`INSERT INTO groups (name) VALUES ('g1')`)
	g2 := one(`INSERT INTO groups (name) VALUES ('g2')`)

	type row struct {
		id        int64
		createdBy *int64
		groups    []int64
	}
	var accounts, proxies []row
	account := func(by *int64, groups ...int64) int64 {
		id := one(`INSERT INTO accounts (name, plugin_key, type, credentials_enc, created_by) VALUES ('a', 'p', 't', '\x00', $1)`, by)
		for _, g := range groups {
			_, err := db.SQL.ExecContext(ctx, `INSERT INTO account_groups (account_id, group_id) VALUES ($1, $2)`, id, g)
			require.NoError(t, err)
		}
		accounts = append(accounts, row{id, by, groups})
		return id
	}
	proxy := func(by *int64) int64 {
		id := one(`INSERT INTO proxies (name, protocol, host, port, created_by) VALUES ('p', 'http', 'proxy.example', 3128, $1)`, by)
		proxies = append(proxies, row{id: id, createdBy: by})
		return id
	}
	a1 := account(&u1, g1)
	a2 := account(&u2, g2)
	a3 := account(nil, g1, g2)
	account(&u1)
	a5 := account(nil)
	p1 := proxy(&u1)
	proxy(&u2)
	proxy(nil)

	scopes := map[string]core.DataScope{
		"all":         core.AllScope,
		"owner":       {Owner: u1},
		"absent user": {Owner: u2 + 100},
		"ids":         {IDs: []int64{a2, a5, p1}},
		"group":       {Groups: []int64{g1}},
		"any group":   {Groups: []int64{0}},
		"any+group":   {Groups: []int64{0, g2}},
		// a3 through g2 and a1 through any group match; a2 is not in g1.
		"pairs": {PairG: []int64{0, g1, g2}, PairID: []int64{a1, a2, a3}},
		"mixed": {Owner: u2, Groups: []int64{g1}, IDs: []int64{a5}, PairG: []int64{g2}, PairID: []int64{a3}},
		"empty": {},
	}
	tables := []struct {
		t        core.ScopeTable
		from, id string
		rows     []row
	}{
		{core.AccountRows, "accounts a", "a.id", accounts},
		{core.ProxyRows, "proxies p", "p.id", proxies},
	}
	for name, s := range scopes {
		for _, tb := range tables {
			var viaGORM []int64
			require.NoError(t, store.G(ctx, db.Gorm).Table(tb.from).Scopes(tb.t.Scope(s)).Order(tb.id).Pluck(tb.id, &viaGORM).Error, name)

			var want []int64
			for _, r := range tb.rows {
				if s.AllowsRow(r.id, r.createdBy, r.groups) {
					want = append(want, r.id)
				}
			}
			assert.ElementsMatch(t, want, viaGORM, "%s %s: GORM scope", name, tb.from)
		}
	}
	assert.Len(t, accounts, 5)
}

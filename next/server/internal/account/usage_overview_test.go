package account

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The usage cell of the account list: totals, today's window and the limit
// of every listed account in one statement.
func TestSQLAccountUsageOverview(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return db.Raw(usageOverviewSQL, store.Int64s{3, 4}, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)).Scan(&struct{}{})
	})
}

// usageOverview reads what usageTotals, usageIn and usageLimits read of the
// same accounts, one statement for the three: accounts with and without
// totals, limits (an anchor and a waived window or none) and hours before
// and after the window's start.
func TestUsageOverviewMatchesItsReads(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a := e.mkRawAccount("anthropic", "apikey", "overview-a", "sk-overview-aaaa", true)
	b := e.mkRawAccount("anthropic", "apikey", "overview-b", "sk-overview-bbbb", true)
	c := e.mkRawAccount("anthropic", "apikey", "overview-c", "sk-overview-cccc", true)
	since := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := e.db.SQL.ExecContext(ctx, sql, args...)
		require.NoError(t, err)
	}
	// Hourly rows arrive through usage_logs and the fold; the overview only
	// reads the tables, so they are written directly here.
	exec(`INSERT INTO account_usage_totals (account_id, cost, raw_cost, requests, tokens) VALUES ($1, 6.5, 5.25, 3, 120), ($2, 0.12345678, 0, 1, 9)`, a, c)
	exec(`INSERT INTO account_usage_hourly (account_id, hour, cost, raw_cost, requests, tokens) VALUES
		($1, $3, 1.5, 1.25, 1, 40), ($1, $3::timestamptz + interval '1 hour', 2, 1, 1, 30), ($1, $3::timestamptz - interval '1 hour', 3, 3, 1, 50),
		($2, $3::timestamptz - interval '2 hours', 0.12345678, 0, 1, 9)`, a, c, since)
	exec(`INSERT INTO account_usage_limits (account_id, raw_cost, enabled, amount, period, period_count, anchor, timezone, waived_window)
		VALUES ($1, true, true, 7.5, 'week', 2, '2026-01-05 03:00:00+00', 'Asia/Shanghai', '2026-11-09 19:00:00+00'),
		       ($2, false, false, 0, 'month', 1, NULL, 'UTC', NULL)`, a, b)

	ids := []int64{c, a, b, 999999}
	totals, windows, limits, err := usageOverview(ctx, e.db.Gorm, ids, since)
	require.NoError(t, err)

	wantTotals, err := usageTotals(ctx, e.db.Gorm, ids)
	require.NoError(t, err)
	wantWindows := make([]usageWindow, len(ids))
	for i, id := range ids {
		wantWindows[i] = usageWindow{account: id, since: since}
	}
	want, err := usageIn(ctx, e.db.Gorm, wantWindows)
	require.NoError(t, err)
	wantLimits, err := usageLimits(ctx, e.db.Gorm, ids)
	require.NoError(t, err)

	require.Equal(t, wantTotals, totals)
	require.Equal(t, want, windows)
	require.Len(t, limits, len(wantLimits))
	for id, l := range wantLimits {
		got, ok := limits[id]
		require.True(t, ok, id)
		require.Equal(t, l.RawCost, got.RawCost)
		require.Equal(t, l.Enabled, got.Enabled)
		require.True(t, l.Amount.Equal(got.Amount), "%s %s", l.Amount, got.Amount)
		require.Equal(t, l.Period, got.Period)
		require.Equal(t, l.PeriodCount, got.PeriodCount)
		require.Equal(t, l.Timezone, got.Timezone)
		require.Equal(t, l.Anchor == nil, got.Anchor == nil)
		if l.Anchor != nil {
			require.True(t, l.Anchor.Equal(*got.Anchor))
		}
		require.Equal(t, l.waived == nil, got.waived == nil)
		if l.waived != nil {
			require.True(t, l.waived.Equal(*got.waived))
		}
	}
	require.Len(t, totals, 2, "accounts without totals have none")
	require.True(t, windows[1].Cost.Equal(decimal.RequireFromString("3.5")) && windows[1].Requests == 2, "the hour before the window is out: %+v", windows[1])
	require.True(t, windows[0].Cost.IsZero() && windows[0].Requests == 0, "%+v", windows[0])
}

package testutil

import (
	"context"
	"strconv"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// InsertUser inserts a user with email and returns its id.
func InsertUser(tb testing.TB, db *store.DB, email string) int64 {
	tb.Helper()
	return insertID(tb, db, `INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email)
}

// InsertGroup inserts a group named name and returns its id.
func InsertGroup(tb testing.TB, db *store.DB, name string) int64 {
	tb.Helper()
	return insertID(tb, db, `INSERT INTO groups (name) VALUES ($1) RETURNING id`, name)
}

// FundUser credits user with a balance no test or benchmark exhausts,
// through the ledger as an admin adjustment.
func FundUser(tb testing.TB, ledger core.Ledger, user int64) {
	tb.Helper()
	if _, err := ledger.Apply(context.Background(), core.LedgerChange{
		UserID: user, Amount: decimal.NewFromInt(1_000_000_000), Credit: true,
		Kind:           "admin_adjust", // billing.KindAdminAdjust; billing's tests import this package
		IdempotencyKey: "testutil-fund-" + strconv.FormatInt(user, 10),
	}); err != nil {
		tb.Fatal(err)
	}
}

func insertID(tb testing.TB, db *store.DB, sql string, args ...any) int64 {
	tb.Helper()
	var id int64
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		tb.Fatal(err)
	}
	return id
}

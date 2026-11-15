package gormtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The models are local on purpose: the snapshots exercise the helper and
// the GORM features the migration relies on, not the generated models.

type usageRow struct {
	ID            int64
	BillingStatus string
}

type userBalance struct {
	UserID    int64 `gorm:"primaryKey;autoIncrement:false"`
	UpdatedAt time.Time
}

func (userBalance) TableName() string { return "user_balances" }

type precharge struct {
	RequestID string
	UserID    int64
}

// An id list is one bigint[] parameter, so "= ANY(?)" keeps one placeholder
// whatever the list's length (and an empty list is not NULL).
func TestSnapshotInt64sArgument(t *testing.T) {
	Assert(t, func(tx *gorm.DB) *gorm.DB {
		var rows []usageRow
		return tx.Table("usage_logs").Select("id", "billing_status").
			Where("user_id = ANY(?)", store.Int64s{3, 1, 2}).
			Where("billing_status = ?", "pending").
			Order("id").Find(&rows)
	})
}

// ensureBalanceSQL's shape: an insert that leaves an existing row alone.
func TestSnapshotOnConflictDoNothing(t *testing.T) {
	at := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	Assert(t, func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoNothing: true}).
			Create(&userBalance{UserID: 7, UpdatedAt: at})
	})
}

// The claim pattern of the sweepers: lock a page of rows, skipping the ones
// another node holds.
func TestSnapshotForUpdateSkipLocked(t *testing.T) {
	Assert(t, func(tx *gorm.DB) *gorm.DB {
		var rows []precharge
		return tx.Table("request_precharges").Select("request_id", "user_id").
			Where("expires_at < now()").
			Order("expires_at").Limit(100).
			Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Find(&rows)
	})
}

// Why store.Int64s exists (and the guard in package gormguard forbids plain
// slices): GORM expands a []int64 argument into one placeholder per element.
func TestPlainSliceIsExpanded(t *testing.T) {
	query, vars, err := Build(func(tx *gorm.DB) *gorm.DB {
		var rows []usageRow
		return tx.Table("usage_logs").Where("id IN ?", []int64{1, 2}).Find(&rows)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "($1,$2)") || len(vars) != 2 {
		t.Fatalf("expected an expanded list, got %q %v", query, vars)
	}
}

func TestBuildNeedsFinisher(t *testing.T) {
	_, _, err := Build(func(tx *gorm.DB) *gorm.DB { return tx.Table("usage_logs").Where("id = ?", 1) })
	if err == nil || !strings.Contains(err.Error(), "finisher") {
		t.Fatalf("err = %v", err)
	}
}

// Anything that reaches the instance's connection pool gets the refusing
// connector, never a server.
func TestDBNeverConnects(t *testing.T) {
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ConnPool.ExecContext(context.Background(), "SELECT 1")
	if !errors.Is(err, errNoConnect) {
		t.Fatalf("err = %v, want %v", err, errNoConnect)
	}
}

func TestRender(t *testing.T) {
	n := int64(5)
	var none *int64
	got := Render(" SELECT 1 ", []any{"", &n, none, nil, store.Strings{"a"}})
	want := "-- sql\nSELECT 1\n-- vars\n$1 string \"\"\n$2 *int64 5\n$3 *int64 nil\n$4 nil\n$5 store.Strings [a]\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

package gormtest

import (
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
	UserID    int64     `gorm:"primaryKey;autoIncrement:false"`
	UpdatedAt time.Time `gorm:"default:(-);autoUpdateTime:false"` // as generated: default now()
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

// How GORM expands slices, as package gormguard documents it: a []any like
// any slice, and empty lists into conditions that differ by form.
func TestExpansionTraps(t *testing.T) {
	for want, cond := range map[string]func(*gorm.DB) *gorm.DB{
		`WHERE id IN ($1,$2)`: func(tx *gorm.DB) *gorm.DB { return tx.Where("id IN ?", []any{1, 2}) },
		// An empty list: no row, as "in the empty list" means.
		`WHERE "usage_logs"."id" IN (NULL)`: func(tx *gorm.DB) *gorm.DB { return tx.Where(map[string]any{"id": []int64{}}) },
		// Not of an empty list in a map: every row whose id is not NULL.
		`WHERE "usage_logs"."id" IS NOT NULL`: func(tx *gorm.DB) *gorm.DB { return tx.Not(map[string]any{"id": []int64{}}) },
		// Not of an empty list as a parameter: no row at all, the opposite
		// of "not in the empty list".
		`WHERE NOT id IN (NULL)`: func(tx *gorm.DB) *gorm.DB { return tx.Not("id IN ?", []int64{}) },
		// Not of an empty list for a column: invalid SQL.
		`WHERE "id" NOT IN ()`: func(tx *gorm.DB) *gorm.DB { return tx.Not("id", []int64{}) },
	} {
		query, _, err := Build(func(tx *gorm.DB) *gorm.DB {
			var rows []usageRow
			return cond(tx.Table("usage_logs")).Find(&rows)
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(query, want) {
			t.Errorf("got %q, want it to end in %q", query, want)
		}
	}
}

func TestBuildNeedsFinisher(t *testing.T) {
	_, _, err := Build(func(tx *gorm.DB) *gorm.DB { return tx.Table("usage_logs").Where("id = ?", 1) })
	if err == nil || !strings.Contains(err.Error(), "finisher") {
		t.Fatalf("err = %v", err)
	}
}

// A statement that escaped DryRun has no Querier to run on, so it never
// reaches a server.
func TestDBNeverConnects(t *testing.T) {
	db := DB()
	_, err := db.ConnPool.ExecContext(db.Statement.Context, "SELECT 1")
	if !errors.Is(err, store.ErrNoQuerier) {
		t.Fatalf("err = %v, want %v", err, store.ErrNoQuerier)
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

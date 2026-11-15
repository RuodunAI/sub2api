package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
)

// The SQL of the account snapshot and settings statements built with GORM
// (see package gormtest). Distinct values show where each argument goes:
// GORM's placeholders are positional, so a value used several times in a
// statement is passed once per use. ApplyAccountQuotaObservation and
// ReadAccountQuotas stay on pgx (hotpath_bench_test.go).

var (
	snapshotTime  = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	snapshotClaim = &store.QuotaClaim{AccountID: 11, Generation: 22, Token: 33, Revision: 44}
)

func TestSQLSettingLock(t *testing.T) {
	var raw []byte
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.LockSetting(db, "gateway").Scan(&raw) })
}

// The update writes updated_at = now(), as the statement it replaces did.
func TestSQLSettingWrite(t *testing.T) {
	uid := int64(7)
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return store.WriteSetting(db, "gateway", json.RawMessage(`{"a":1}`), &uid)
	})
}

func TestSQLBalanceUpsert(t *testing.T) {
	b := &store.BalanceSnapshot{AccountID: 11, AmountMicros: 5, Currency: "USD", Source: store.BalanceActive, UpdatedAt: &snapshotTime}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.UpsertBalanceSQL(db, b) })
}

func TestSQLRefreshRecord(t *testing.T) {
	s := store.RefreshState{AccountID: 11, ExpiresAt: &snapshotTime, ErrorType: store.RefreshTransient, Error: "boom", RejectedCredHash: []byte{1, 2}}
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.RecordRefreshSQL(db, s) })
}

func TestSQLQuotaSave(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return store.SaveAccountQuotaSQL(db, 11, store.QuotaPassive, `{"5h":{}}`, &snapshotTime)
	})
}

func TestSQLQuotaClaimQuery(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.ClaimQuotaQuerySQL(db, 11, 2.5) })
}

func TestSQLQuotaSetError(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.SetQuotaErrorSQL(db, 11, "boom") })
}

func TestSQLQuotaEnsure(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.EnsureQuotaSQL(db, 11, "v1", true) })
}

func TestSQLQuotaReset(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.ResetQuotaSQL(db, 11) })
}

func TestSQLQuotaLease(t *testing.T) {
	var id int64
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.LeaseQuotaSQL(db, 11, "v1", 2.5, 70, false).Scan(&id) })
}

func TestSQLQuotaRelease(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB { return store.ReleaseQuotaSQL(db, snapshotClaim, snapshotTime) })
}

func TestSQLQuotaComplete(t *testing.T) {
	gormtest.Assert(t, func(db *gorm.DB) *gorm.DB {
		return store.CompleteQuotaSQL(db, snapshotClaim, `{"5h":{}}`, "boom", snapshotTime)
	})
}

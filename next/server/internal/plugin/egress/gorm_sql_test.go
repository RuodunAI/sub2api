package egress

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL snapshots of the egress statements (see package gormtest).

var (
	sqlFrom = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	sqlTo   = sqlFrom.Add(time.Hour)
)

func TestSQLResetOpenLogs(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return resetOpenLogs(tx, "node-1", sqlFrom) })
}

// The log rows go in as one statement of twelve array parameters, numbered
// by their position: the open ones return their ids by it, the finished
// ones (whose closed_at is their start plus duration) return nothing.
func TestSQLInsertLogRows(t *testing.T) {
	rows := []logEntry{
		{PluginKey: "demo", NodeID: "node-1", Network: "tcp", Host: "api.example.com", Port: 443, StartedAt: sqlFrom, Result: ResultOpen},
		{PluginKey: "demo", NodeID: "node-1", Network: "tcp", Host: "api.example.com", Port: 443, StartedAt: sqlFrom,
			DurationMS: 1500, BytesIn: 10, Result: ResultOK},
	}
	t.Run("open", func(t *testing.T) {
		gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return insertOpenRows(tx, rows[:1]).Scan(&[]int64{}) })
	})
	t.Run("done", func(t *testing.T) {
		gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return insertDoneRows(tx, rows) })
	})
	// The text does not depend on how many rows there are.
	one, _, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB { return insertDoneRows(tx, rows[:1]) })
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := gormtest.Build(func(tx *gorm.DB) *gorm.DB { return insertDoneRows(tx, rows) })
	if err != nil || one != two {
		t.Fatalf("the statement changes with the batch: %q, %q, %v", one, two, err)
	}
}

// The int32 wrap of a duration happens in Go, before the array is built.
func TestSQLCloseLogRows(t *testing.T) {
	rows := []closedRow{
		{ID: 7, Result: ResultOK, DurationMS: 1500, BytesIn: 10, BytesOut: 20, ClosedAt: sqlTo},
		{ID: 8, Result: "dial_error", Error: "refused", ClosedAt: sqlTo},
	}
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return closeLogRows(tx, rows) })
}

func TestSQLUpsertDomainRows(t *testing.T) {
	ds := []domainDelta{{PluginKey: "demo", Host: "api.example.com", FirstSeen: sqlFrom, LastSeen: sqlTo, Connections: 3}}
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return upsertDomainRows(tx, ds).Scan(&[]upsertedDomain{}) })
}

func TestSQLDomains(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return domainsQuery(tx, "demo").Scan(&[]DomainRecord{}) })
}

func TestSQLSummary(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return summaryQuery(tx, "demo", sqlFrom, sqlTo).Scan(&[]HostSummary{})
	})
}

func TestSQLList(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return listQuery(tx, "demo", sqlFrom, sqlTo, 200).Find(&[]model.PluginEgressLog{})
	})
}

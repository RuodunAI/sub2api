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

// The log rows go in as one multi-row INSERT that returns their ids: an
// open row and a finished one, whose zero bytes and error take the column
// defaults (0 and the empty string).
func TestSQLInsertLogRows(t *testing.T) {
	closed := sqlTo
	logs := []model.PluginEgressLog{
		{PluginKey: "demo", NodeID: "node-1", Network: "tcp", Host: "api.example.com", Port: 443, StartedAt: sqlFrom, Result: "open"},
		{PluginKey: "demo", NodeID: "node-1", Network: "tcp", Host: "api.example.com", Port: 443, StartedAt: sqlFrom,
			DurationMs: 1500, BytesIn: 10, Result: ResultOK, ClosedAt: &closed},
	}
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return insertLogRows(tx, logs) })
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

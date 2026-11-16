package task

import (
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// The SQL snapshots of the task statements (see package gormtest).

// Every use of a named parameter is a parameter of its own, all with the
// value of the name.
func TestSQLRecordRun(t *testing.T) {
	o := runOutput{Status: StatusFailed, Count: 3, Error: "upstream down"}
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return recordRun(tx, "core.test.bad", 42, "node-a", StatusFailed, o)
	})
}

// The registered names are one text[] parameter; none is the empty array.
func TestSQLRemoveStaleCoreTasks(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return removeStaleCoreTasks(tx, []string{"core.a", "core.b"})
	})
}

func TestSQLRunsQuery(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return runsQuery(tx, "core.test.ok", []string{StatusFailed, StatusTimeout}, 120, 21)
	})
}

// The refresh of the registered core tasks leaves updated_at alone.
func TestSQLMarkSeen(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return markSeen(tx, []string{"core.a", "core.b"}, "1.2.3") })
}

// A put-off run frees its own lease by the statement clock; housekeeping
// frees the expired ones by the transaction's.
func TestSQLReleaseLeases(t *testing.T) {
	t.Run("put off", func(t *testing.T) {
		gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
			return releaseLeases(tx.Where("name = ? AND lease_job_id = ?", "core.test", int64(42)), gorm.Expr("clock_timestamp()"))
		})
	})
	t.Run("expired", func(t *testing.T) {
		gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return releaseLeases(tx.Where("lease_until < now()"), gorm.Expr("now()")) })
	})
}

// false is written (a map, not a struct) and only the source comes back.
func TestSQLSetPluginTaskEnabled(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		var row model.ScheduledTask
		return setPluginTaskEnabled(tx, "plugin.demo.sync", false, &row)
	})
}

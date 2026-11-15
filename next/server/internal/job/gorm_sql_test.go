package job

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/gormtest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/task"
)

// The SQL snapshots of the job statements (see package gormtest).

// The slot claim repeats key, job and slot: once for the row, once for the
// NOT EXISTS check.
func TestSQLClaimSlot(t *testing.T) {
	slot := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB {
		return claimSlot(tx, "demo", "sync", "node-a", core.TaskCluster, slot)
	})
}

func TestSQLUpsertManifestTask(t *testing.T) {
	tk := &task.Task{
		TaskSpec:     core.TaskSpec{Name: TaskName("demo", "sync"), Timeout: 90 * time.Second, MaxAttempts: 3},
		Source:       task.SourcePluginManifest,
		PluginKey:    "demo",
		ScheduleText: "@every 5m",
	}
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return upsertManifestTask(tx, tk, "") })
}

// The failed status, finished_at and the message are one SET list.
func TestSQLAbandonStaleRuns(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return abandonStaleRuns(tx, 2*time.Hour) })
}

func TestSQLTrimRuns(t *testing.T) {
	gormtest.Assert(t, func(tx *gorm.DB) *gorm.DB { return trimRuns(tx, 1000) })
}

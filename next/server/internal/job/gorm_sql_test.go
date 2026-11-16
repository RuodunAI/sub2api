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

// The slot claim leaves a taken slot (the partial slot index) as it is.
func TestSQLClaimSlot(t *testing.T) {
	run := core.TaskRun{ScheduledAt: time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)}
	gormtest.AssertRun(t, func(tx *gorm.DB) error {
		_, err := insertRunRow(tx, "demo", "sync", "node-a", core.TaskCluster, run, true)
		return err
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

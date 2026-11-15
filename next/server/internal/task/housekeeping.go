package task

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// HousekeepingTask is the framework's own cluster task (TASK-FRAMEWORK-DESIGN
// §8.1).
const HousekeepingTask = "core.task.housekeeping"

// staleCoreTask is how long a core task's row survives once no node
// registers it any more.
const staleCoreTask = 7 * 24 * time.Hour

func (s *Service) housekeepingSpec() core.TaskSpec {
	return core.TaskSpec{
		Name:        HousekeepingTask,
		Title:       core.LocalizedText{"en": "Scheduled task housekeeping", "zh": "定时任务自身维护"},
		Description: core.LocalizedText{"en": "Removes the records of core tasks no node registers any more and clears expired leases.", "zh": "删除已不再登记的核心任务记录，清理过期租约。"},
		Scope:       core.TaskCluster,
		Every:       time.Hour,
		Timeout:     time.Minute,
		Handler:     s.housekeeping,
	}
}

func (s *Service) housekeeping(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
	db := s.opts.DB
	names := []string{}
	for _, t := range s.reg.List(core.TaskCluster) {
		if t.Source == SourceCore {
			names = append(names, t.Name)
		}
	}
	g := store.G(ctx, db.Gorm)
	if err := markSeen(g, names, s.opts.Version).Error; err != nil {
		return core.TaskResult{}, fmt.Errorf("refresh registered tasks: %w", err)
	}
	removed := removeStaleCoreTasks(g, names)
	if removed.Error != nil {
		return core.TaskResult{}, fmt.Errorf("remove stale tasks: %w", removed.Error)
	}
	cleared := releaseLeases(g, "now()", "lease_until < now()")
	if cleared.Error != nil {
		return core.TaskResult{}, fmt.Errorf("clear expired leases: %w", cleared.Error)
	}
	n := int(removed.RowsAffected + cleared.RowsAffected)
	return core.TaskResult{Count: n, Message: fmt.Sprintf("removed %d stale tasks, cleared %d expired leases",
		removed.RowsAffected, cleared.RowsAffected)}, nil
}

// markSeen records that a node of version registers the core tasks names.
// It writes seen_at and seen_version alone: updated_at stays, as the raw
// SQL had it.
func markSeen(g *gorm.DB, names []string, version string) *gorm.DB {
	return g.Model(&model.ScheduledTask{}).Where("source = 'core' AND name = ANY(?)", store.Strings(names)).
		Updates(map[string]any{"seen_at": gorm.Expr("now()"), "seen_version": version})
}

// removeStaleCoreTasks deletes the rows of core tasks that no node has
// registered for staleCoreTask, other than registered, and that no run
// holds.
func removeStaleCoreTasks(g *gorm.DB, registered []string) *gorm.DB {
	return g.Where(`source = 'core' AND seen_at < now() - ?::interval AND NOT (name = ANY(?))
			AND (lease_until IS NULL OR lease_until < now())`, staleCoreTask.String(), store.Strings(registered)).
		Delete(&model.ScheduledTask{})
}

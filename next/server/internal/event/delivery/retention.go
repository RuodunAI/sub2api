package delivery

import (
	"context"
	"math"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

const retentionChunk = 5000

// RetentionTask is the cluster task that purges delivered events.
const RetentionTask = "core.events.retention"

// RetentionTaskSpec runs PurgeEvents every RetentionInterval on one node.
func (s *Service) RetentionTaskSpec() core.TaskSpec {
	return core.TaskSpec{
		Name:  RetentionTask,
		Title: core.LocalizedText{"en": "Purge delivered events", "zh": "清理 7 天前事件"},
		Description: core.LocalizedText{
			"en": "Deletes events older than the retention age that every plugin subscription has already received.",
			"zh": "删除超过保留期且所有插件订阅都已收到的事件。",
		},
		Scope:   core.TaskCluster,
		Every:   s.opts.RetentionInterval,
		Timeout: 5 * time.Minute,
		Handler: func(ctx context.Context, _ core.TaskRun) (core.TaskResult, error) {
			n, err := s.PurgeEvents(ctx)
			return core.TaskResult{Count: int(n)}, err
		},
	}
}

// PurgeEvents deletes events older than RetentionAge that every plugin
// cursor has passed.
func (s *Service) PurgeEvents(ctx context.Context) (int64, error) {
	var total int64
	for {
		res := purgeChunk(store.G(ctx, s.db.Gorm), s.opts.RetentionAge)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected < retentionChunk {
			return total, nil
		}
	}
}

// purgeChunk deletes up to retentionChunk events older than age that every
// plugin cursor has passed: a DELETE bounded by a LIMIT subquery.
func purgeChunk(g *gorm.DB, age time.Duration) *gorm.DB {
	passed := g.Model(&model.PluginEventCursor{}).Select("COALESCE(min(last_event_id), ?)", int64(math.MaxInt64))
	chunk := g.Model(&model.Event{}).Select("id").
		Where("occurred_at < now() - make_interval(secs => ?)", age.Seconds()).
		Where("id <= (?)", passed).Order("id").Limit(retentionChunk)
	return g.Where("id IN (?)", chunk).Delete(&model.Event{})
}

package account

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func markLifecyclePending(ctx context.Context, q *gorm.DB, id int64) error {
	g := store.G(ctx, q)
	// A new generation of the account's synchronization, pending.
	if err := g.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoUpdates: clause.Assignments(map[string]any{
		"status": "pending", "code": "", "generation": gorm.Expr("account_lifecycle_sync.generation+1"), "updated_at": gorm.Expr("now()"),
	})}).Create(&model.AccountLifecycleSync{AccountID: id, Status: "pending"}).Error; err != nil {
		return err
	}
	// The account's plugin gets a cursor at the latest event, if it has none.
	cursor := g.Model(&model.Account{}).Select("plugin_key, (?)", g.Model(&model.Event{}).Select("COALESCE(max(id),0)")).Where("id = ?", id)
	return store.InsertSelect(g, "plugin_event_cursors", []string{"plugin_key", "last_event_id"}, cursor,
		clause.OnConflict{Columns: []clause.Column{{Name: "plugin_key"}}, DoNothing: true}).Error
}

// saveLifecycleStatus records state for the account version and generation
// that it was read for: a newer save or pending mark wins.
func (s *Service) saveLifecycleStatus(ctx context.Context, id int64, version time.Time, generation int64, state core.AccountSynchronization) error {
	g := store.G(ctx, s.d.DB.Gorm)
	row := g.Model(&model.Account{}).Select("id, ?, ?", state.Status, state.Code).Where("id = ? AND updated_at = ?", id, version)
	return store.InsertSelect(g, "account_lifecycle_sync", []string{"account_id", "status", "code"}, row, clause.OnConflict{
		Columns:   []clause.Column{{Name: "account_id"}},
		DoUpdates: append(clause.AssignmentColumns([]string{"status", "code"}), clause.Assignment{Column: clause.Column{Name: "updated_at"}, Value: gorm.Expr("now()")}),
		Where:     clause.Where{Exprs: []clause.Expression{gorm.Expr("account_lifecycle_sync.generation = ?", generation)}},
	}).Error
}

func (s *Service) fillLifecycleStatus(ctx context.Context, views []*View) error {
	ids := make([]int64, 0, len(views))
	byID := make(map[int64]*View, len(views))
	for _, view := range views {
		binding, ok := s.accountType(view.PluginKey, view.Type)
		if ok && binding.Type.Lifecycle != nil {
			ids = append(ids, view.ID)
			byID[view.ID] = view
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var states []model.AccountLifecycleSync
	if err := store.G(ctx, s.d.DB.Gorm).Select("account_id, status, code").Where("account_id = ANY(?)", store.Int64s(ids)).
		Find(&states).Error; err != nil {
		return err
	}
	for _, l := range states {
		byID[l.AccountID].LifecycleSync = &core.AccountSynchronization{Status: l.Status, Code: l.Code}
	}
	return nil
}

// Only event delivery wrote the synchronization state, so an account left
// pending or failed stayed so after its runtime came up on a later reconcile
// pass, and quota refresh skipped it until the snapshot went stale
// (2026-10-21 audit). The sweep reads the resource again for those rows.
const (
	lifecycleStatusSweepEvery = time.Minute
	// lifecycleStatusSettle leaves a fresh state to the delivery in flight.
	lifecycleStatusSettle    = 30 * time.Second
	lifecycleStatusSweepPage = 50
)

// sweepLifecycleStatus rereads the synchronization of accounts not applied
// for a while and returns how many changed. Every read is written back, so
// unchanged rows move behind the others (oldest first) instead of filling
// every page; a read that fails leaves the row as it is.
func (s *Service) sweepLifecycleStatus(ctx context.Context) (int, error) {
	var accounts []staleLifecycle
	if err := store.G(ctx, s.d.DB.Gorm).Table("account_lifecycle_sync l").
		Select("a.id, a.plugin_key, a.type, a.updated_at, l.generation, l.status, l.code").Joins("JOIN accounts a ON a.id = l.account_id").
		Where("l.status <> 'applied' AND l.updated_at <= now() - make_interval(secs => ?::float8) AND a.deleted_at IS NULL", lifecycleStatusSettle.Seconds()).
		Order("l.updated_at, a.id").Limit(lifecycleStatusSweepPage).Scan(&accounts).Error; err != nil {
		return 0, err
	}
	updated := 0
	for _, a := range accounts {
		binding, ok := s.accountType(a.PluginKey, a.Type)
		reader, readable := s.d.LifecycleResources[a.PluginKey].(core.AccountSynchronizationReader)
		if !ok || binding.Type.Lifecycle == nil || !readable {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 30*time.Second)
		state, err := reader.AccountSynchronization(call, a.ID)
		cancel()
		if err != nil {
			continue
		}
		if err := s.saveLifecycleStatus(ctx, a.ID, a.UpdatedAt, a.Generation, state); err != nil {
			return updated, err
		}
		if state != (core.AccountSynchronization{Status: a.Status, Code: a.Code}) {
			updated++
		}
	}
	return updated, nil
}

// staleLifecycle is a synchronization sweepLifecycleStatus reads again,
// with the account version it is for.
type staleLifecycle struct {
	ID         int64
	PluginKey  string
	Type       string
	UpdatedAt  time.Time
	Generation int64
	Status     string
	Code       string
}

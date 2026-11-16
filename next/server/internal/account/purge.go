package account

import (
	"context"
	"encoding/json"
	"log/slog"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var _ core.PluginAccountPurger = (*Service)(nil)

// PurgePluginAccounts implements core.PluginAccountPurger (CONTRACTS §14.3):
// it soft-deletes every account of the plugin's account types, removes their
// group memberships and emits one account.deleted event per account, all in
// one transaction; then it clears their cooldowns and broadcasts
// account:changed once. Returns the number of accounts deleted.
func (s *Service) PurgePluginAccounts(ctx context.Context, pluginKey string) (int, error) {
	if pluginKey == "" {
		return 0, core.ErrInvalidArgument.WithMessage("plugin key is required")
	}
	var ids []int64
	err := s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		ids = ids[:0]
		g := store.G(ctx, tx)
		var purged []model.Account
		if err := g.Model(&purged).Clauses(clause.Returning{Columns: []clause.Column{{Name: "id"}, {Name: "type"}, {Name: "name"}}}).
			Where("plugin_key = ? AND deleted_at IS NULL", pluginKey).
			Updates(map[string]any{"deleted_at": gorm.Expr("now()"), "updated_at": gorm.Expr("now()")}).Error; err != nil {
			return err
		}
		var evs []core.Event
		for _, a := range purged {
			ids = append(ids, a.ID)
			evs = append(evs, core.Event{Type: core.EventAccountDeleted, Payload: basicPayload(a.ID, pluginKey, a.Type, a.Name)})
		}
		if len(ids) == 0 {
			return nil
		}
		if err := g.Where("account_id = ANY(?)", store.Int64s(ids)).Delete(&model.AccountGroup{}).Error; err != nil {
			return err
		}
		if _, err := s.d.ScopeGrants.ClearScopeGrants(ctx, tx, 0, core.ScopeRefAccount, ids); err != nil {
			return err
		}
		return s.d.Events.Emit(ctx, tx, evs...)
	})
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if s.d.Redis != nil {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = cooldownKey(id)
		}
		if err := s.d.Redis.Del(ctx, keys...).Err(); err != nil {
			slog.WarnContext(ctx, "account: clear cooldowns of purged accounts", "plugin_key", pluginKey, "err", err)
		}
	}
	s.purged(ctx, pluginKey, len(ids))
	slog.InfoContext(ctx, "account: purged plugin accounts", "plugin_key", pluginKey, "count", len(ids))
	return len(ids), nil
}

// purged invalidates local caches and
// broadcasts one account:changed for a bulk deletion.
func (s *Service) purged(ctx context.Context, pluginKey string, n int) {
	s.invalidateNode(0)
	if s.d.Bus == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"reason": "plugin_purged", "plugin_key": pluginKey, "count": n})
	if err := s.d.Bus.Publish(ctx, core.ChannelAccountChanged, b); err != nil {
		slog.WarnContext(ctx, "account: publish account:changed", "err", err)
	}
}

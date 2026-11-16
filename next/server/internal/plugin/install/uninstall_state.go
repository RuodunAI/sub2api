package install

import (
	"context"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type UninstallState struct {
	Epoch          int64     `json:"epoch"`
	TargetBootIDs  []string  `json:"target_boot_ids"`
	StoppedBootIDs []string  `json:"stopped_boot_ids"`
	PendingBootIDs []string  `json:"pending_boot_ids"`
	RequestedAt    time.Time `json:"requested_at"`
}

func readUninstallState(ctx context.Context, q *gorm.DB, key string, lock bool) (*UninstallState, error) {
	if lock {
		if err := store.RequireTx(q); err != nil {
			return nil, err
		}
	}
	var row model.PluginUninstall
	err := uninstallState(store.G(ctx, q), key, lock).Take(&row).Error
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound.WithMessage("no uninstall is pending for this plugin")
	}
	if err != nil {
		return nil, err
	}
	out := &UninstallState{Epoch: row.Epoch, TargetBootIDs: row.TargetBootIDs, StoppedBootIDs: row.StoppedBootIDs, RequestedAt: row.RequestedAt}
	set := map[string]bool{}
	for _, id := range out.StoppedBootIDs {
		set[id] = true
	}
	out.PendingBootIDs = []string{}
	for _, id := range out.TargetBootIDs {
		if !set[id] {
			out.PendingBootIDs = append(out.PendingBootIDs, id)
		}
	}
	sort.Strings(out.TargetBootIDs)
	sort.Strings(out.StoppedBootIDs)
	sort.Strings(out.PendingBootIDs)
	return out, nil
}

// uninstallState selects the uninstall barrier of key, FOR UPDATE when lock.
func uninstallState(db *gorm.DB, key string, lock bool) *gorm.DB {
	db = uninstallRow(db, key).Select("epoch, target_boot_ids, stopped_boot_ids, requested_at")
	if lock {
		db = db.Clauses(store.ForUpdate())
	}
	return db
}

func (s *Service) UninstallState(ctx context.Context, key string) (*UninstallState, error) {
	return readUninstallState(ctx, s.d.DB.Gorm, key, false)
}

type ConfirmStoppedRequest struct {
	Epoch   int64    `json:"epoch"`
	BootIDs []string `json:"boot_ids"`
	Reason  string   `json:"reason"`
}

// ConfirmUninstallStopped records an administrator's explicit assertion that
// named processes have been physically terminated. Missing heartbeats alone
// never prove that. It only acknowledges the current barrier, never purges.
func (s *Service) ConfirmUninstallStopped(ctx context.Context, key string, in ConfirmStoppedRequest, actorID int64) (*UninstallState, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Epoch <= 0 || len(in.BootIDs) == 0 || len(in.BootIDs) > 1024 || in.Reason == "" || len(in.Reason) > 2000 {
		return nil, core.ErrInvalidArgument.WithMessage("current epoch, boot IDs and a nonempty physical-stop explanation are required")
	}
	ids := map[string]bool{}
	for _, id := range in.BootIDs {
		if id == "" || strings.TrimSpace(id) != id || len(id) > 128 {
			return nil, core.ErrInvalidArgument.WithMessage("invalid boot ID")
		}
		ids[id] = true
	}
	if s.d.Nodes == nil {
		return nil, core.ErrUnavailable.WithMessage("node registry is required for stop confirmation")
	}
	var out *UninstallState
	err := s.d.DB.Tx(ctx, func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		var keyLocked string
		if err := lockedPlugin(g, key).Select("key").Take(&keyLocked).Error; err != nil {
			if store.IsNoRows(err) {
				return core.ErrNotFound.WithMessage("plugin not found")
			}
			return err
		}
		state, err := readUninstallState(ctx, tx, key, true)
		if err != nil {
			return err
		}
		if in.Epoch != state.Epoch {
			return core.ErrConflict.WithMessage("uninstall epoch changed; refresh its state before confirming")
		}
		targets := map[string]bool{}
		for _, id := range state.TargetBootIDs {
			targets[id] = true
		}
		for id := range ids {
			if !targets[id] {
				return core.ErrInvalidArgument.WithMessage("boot ID is not a target of this uninstall")
			}
		}
		live, err := s.d.Nodes.LiveNodes(ctx)
		if err != nil {
			return core.ErrUnavailable.WithMessage("cannot verify current live nodes").WithCause(err)
		}
		for _, node := range live {
			if ids[node.BootID] {
				return core.ErrConflict.WithMessage("a selected boot is still visible as live; physically stop it first")
			}
		}
		confirmed := make([]string, 0, len(ids))
		for id := range ids {
			confirmed = append(confirmed, id)
		}
		sort.Strings(confirmed)
		boots := store.Strings(confirmed)
		if err = addStoppedBoots(g, key, boots).Error; err != nil {
			return err
		}
		if err = markNodesStopped(g, key, boots).Error; err != nil {
			return err
		}
		if err = markCleanupsCleaned(g, key, boots).Error; err != nil {
			return err
		}
		if err = audit.Audit(ctx, tx, actorID, "plugin.uninstall.confirm_stopped", "plugin", key, map[string]any{
			"epoch": in.Epoch, "boot_ids": confirmed, "reason": in.Reason, "administrator_confirmed_physical_stop": true,
		}); err != nil {
			return err
		}
		out, err = readUninstallState(ctx, tx, key, false)
		return err
	})
	return out, err
}

// addStoppedBoots adds boots to the stopped boots of key's uninstall.
func addStoppedBoots(db *gorm.DB, key string, boots store.Strings) *gorm.DB {
	return uninstallRow(db, key).
		Update("stopped_boot_ids", gorm.Expr("ARRAY(SELECT DISTINCT unnest(stopped_boot_ids||?::text[]))", boots))
}

// markNodesStopped marks key's runtime nodes of boots stopped.
func markNodesStopped(db *gorm.DB, key string, boots store.Strings) *gorm.DB {
	return db.Model(&model.PluginRuntimeNode{}).Where("plugin_key = ? AND boot_id = ANY(?::text[])", key, boots).
		Updates(map[string]any{"stopped": true, "updated_at": gorm.Expr("now()")})
}

// markCleanupsCleaned marks the pending cleanups of key's rollouts on boots
// done: the boots stopped, so nothing is left to clean there.
func markCleanupsCleaned(db *gorm.DB, key string, boots store.Strings) *gorm.DB {
	return db.Exec(`UPDATE plugin_rollout_cleanup n SET state='cleaned',updated_at=now() FROM plugin_rollouts r WHERE r.id=n.rollout_id AND r.plugin_key=? AND n.boot_id=ANY(?::text[]) AND n.state='cleanup_pending'`, key, boots)
}

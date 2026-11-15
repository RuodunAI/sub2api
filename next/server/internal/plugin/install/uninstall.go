package install

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// running reports whether a status means instances may be serving.
func running(status string) bool {
	return status == StatusEnabled || status == StatusEnabling || status == StatusUpgrading
}

// UninstallOptions select what uninstall removes besides the plugin row.
type UninstallOptions struct {
	// Purge drops the plugin's database schema and deletes its KV entries.
	Purge bool
	// PurgeAccounts deletes the accounts of the plugin's account types
	// through Deps.Accounts; otherwise they are kept and show up orphaned.
	PurgeAccounts bool
}

// UninstallResult reports what uninstall removed.
type UninstallResult struct {
	AccountsDeleted int `json:"accounts_deleted"`
}

// Uninstall disables the plugin if needed, optionally drops its schema, and
// deletes the plugin row (cascading grants, versions, cursors, job runs,
// rollouts, plugin-default prices and sticky rules) and its egress domains. Accounts are kept and
// show up as orphaned unless opt.PurgeAccounts is set. The key stays with the
// plugin's publisher (retired.go).
func (s *Service) Uninstall(ctx context.Context, key string, opt UninstallOptions, actorID int64) (UninstallResult, error) {
	var res UninstallResult
	if builtin, err := IsBuiltin(ctx, s.d.DB.Pool, key); err != nil {
		return res, err
	} else if builtin {
		return res, ErrBuiltin
	}
	if opt.PurgeAccounts && s.d.Accounts == nil {
		return res, core.ErrUnavailable.WithMessage("account purging is unavailable on this node")
	}
	if err := s.quiesce(ctx, key, actorID); err != nil {
		return res, err
	}
	purge := opt.Purge
	err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		g := store.G(ctx, tx)
		var st string
		err := lockedPlugin(g, key).Select("status").Take(&st).Error
		if store.IsNoRows(err) {
			return core.ErrNotFound.WithMessage("plugin not found")
		}
		if err != nil {
			return err
		}
		if running(st) {
			return core.ErrConflict.WithMessage("the plugin was re-enabled concurrently")
		}
		var stopped bool
		if err := uninstallRow(g, key).Select("target_boot_ids <@ stopped_boot_ids").Clauses(forUpdate).Take(&stopped).Error; err != nil {
			return err
		}
		if !stopped {
			return core.ErrConflict.WithMessage("plugin nodes have not all stopped")
		}
		if opt.PurgeAccounts {
			// Keep the plugin row and uninstall barrier locked until account
			// cleanup ends: another uninstall/reinstall cannot create a new
			// lifecycle whose accounts would be swept by this old request.
			n, err := s.d.Accounts.PurgePluginAccounts(ctx, key)
			res.AccountsDeleted = n
			if err != nil {
				return core.ErrInternal.WithMessage("plugin uninstall is pending: deleting its accounts failed; retry uninstall").WithCause(err)
			}
			if err := audit.Audit(ctx, tx, actorID, "plugin.accounts.purge", "plugin", key, map[string]any{"accounts_deleted": n, "ok": true}); err != nil {
				return err
			}
		}
		if purge && s.d.Schemas != nil {
			if err := s.d.Schemas.DropTx(ctx, tx, key); err != nil {
				return fmt.Errorf("drop plugin schema: %w", err)
			}
		}
		if s.d.Permissions != nil {
			if err := s.d.Permissions.DeletePlugin(ctx, tx, key); err != nil {
				return err
			}
		}
		if err := retireKey(ctx, tx, key, actorID); err != nil {
			return err
		}
		if r := g.Where("key = ? AND NOT builtin", key).Delete(&model.Plugin{}); r.Error != nil {
			return r.Error
		} else if r.RowsAffected == 0 {
			return ErrBuiltin
		}
		// No foreign key to plugins: a reinstalled plugin must raise
		// plugin.egress_new_domain again for the hosts it connects to.
		if err := g.Where("plugin_key = ?", key).Delete(&model.PluginEgressDomain{}).Error; err != nil {
			return err
		}
		return audit.Audit(ctx, tx, actorID, "plugin.uninstall", "plugin", key, map[string]any{
			"purge": purge, "purge_accounts": opt.PurgeAccounts, "previous_status": st})
	})
	if err != nil {
		return res, err
	}
	if purge {
		s.purgeKV(ctx, key)
	}
	s.Notify(ctx, key)
	return res, nil
}

func checkUninstall(ctx context.Context, q store.Querier, key string) error {
	g := store.G(ctx, q)
	var pending bool
	if err := selectExists(g, uninstallRow(g, key)).Scan(&pending).Error; err != nil {
		return err
	}
	if pending {
		return core.ErrConflict.WithMessage("plugin uninstall is waiting for nodes to stop")
	}
	return nil
}

// quiesce leaves a durable barrier if the request is interrupted. Never infer
// that a process stopped solely from a missing heartbeat: a partitioned node
// can still have a live plugin and an independent database connection.
func (s *Service) quiesce(ctx context.Context, key string, actorID int64) error {
	if s.d.Nodes == nil {
		return core.ErrUnavailable.WithMessage("node registry is required for uninstall")
	}
	boots := func(readCtx context.Context) ([]string, error) {
		nodes, err := s.d.Nodes.LiveNodes(readCtx)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(nodes))
		for _, n := range nodes {
			ids = append(ids, n.BootID)
		}
		return ids, nil
	}
	ids, err := boots(ctx)
	if err != nil {
		return err
	}
	err = func() error {
		ctx, release, err := core.BeginPluginMutation(ctx, s.d.Mutations)
		if err != nil {
			return err
		}
		defer release()
		return s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
			g := store.G(ctx, tx)
			var p model.Plugin
			if err := lockedPlugin(g, key).Select("row_version, builtin").Take(&p).Error; err != nil {
				return err
			}
			if p.Builtin {
				return ErrBuiltin
			}
			var open bool
			if err := selectExists(g, g.Model(&model.PluginRollout{}).Where("plugin_key = ? AND phase IN ('preparing','activating')", key)).
				Scan(&open).Error; err != nil {
				return err
			}
			if open {
				return core.ErrConflict.WithMessage("a rollout is in progress; cancel it or wait until it finishes")
			}
			r := beginUninstall(g, key, p.RowVersion+1, ids)
			if r.Error != nil || r.RowsAffected == 0 {
				return r.Error
			}
			if err := disableForUninstall(g, key).Error; err != nil {
				return err
			}
			if s.d.Permissions != nil {
				if err := s.d.Permissions.SetPluginActive(ctx, tx, key, false); err != nil {
					return err
				}
			}
			return audit.Audit(ctx, tx, actorID, "plugin.uninstall.begin", "plugin", key, nil)
		})
	}()
	if err != nil {
		return err
	}
	s.Notify(ctx, key)
	waitCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		ids, err = boots(waitCtx)
		if err != nil {
			return err
		}
		var stopped bool
		// Include nodes that joined while the marker was being committed. A
		// node starting after it can only read the disabled desired state.
		err = addUninstallTargets(store.G(waitCtx, s.d.DB.Pool), key, ids).Row().Scan(&stopped)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return core.ErrConflict.WithMessage("plugin uninstall is pending: waiting for all nodes to acknowledge stop; retry uninstall")
		case <-t.C:
		}
	}
}

// uninstallRow is the plugin_uninstalls row of key: the uninstall barrier.
func uninstallRow(db *gorm.DB, key string) *gorm.DB {
	return db.Model(&model.PluginUninstall{}).Where("plugin_key = ?", key)
}

// beginUninstall leaves the uninstall barrier of key, unless one is up,
// targeting the live boots and the nodes still running the plugin.
func beginUninstall(db *gorm.DB, key string, epoch int64, boots []string) *gorm.DB {
	return db.Exec(`INSERT INTO plugin_uninstalls(plugin_key,epoch,target_boot_ids)
			VALUES(?::text,?,ARRAY(SELECT DISTINCT unnest(?::text[] || ARRAY(SELECT boot_id FROM plugin_runtime_nodes WHERE plugin_key=?::text AND NOT stopped)))) ON CONFLICT DO NOTHING`,
		key, epoch, store.Strings(boots), key)
}

// disableForUninstall disables key and clears its desired version while
// its uninstall waits for the nodes to stop.
func disableForUninstall(db *gorm.DB, key string) *gorm.DB {
	return pluginRow(db, key).Updates(map[string]any{
		"status":          StatusDisabled,
		"desired_version": nil,
		"status_reason":   "uninstalling",
		"row_version":     gorm.Expr("row_version + 1"),
		"updated_at":      gorm.Expr("now()"),
	})
}

// addUninstallTargets adds boots to the targets of key's uninstall and
// selects whether every target has stopped.
func addUninstallTargets(db *gorm.DB, key string, boots []string) *gorm.DB {
	return db.Raw(`UPDATE plugin_uninstalls SET target_boot_ids=ARRAY(SELECT DISTINCT unnest(target_boot_ids || ?::text[]))
			WHERE plugin_key=? RETURNING target_boot_ids <@ stopped_boot_ids`, store.Strings(boots), key)
}

// disableRevoked disables running plugins after a revocation (best effort).

func (s *Service) disableRevoked(ctx context.Context, keys []string, actorID int64, reason string) []string {
	var disabled []string
	for _, k := range keys {
		if s.d.Rollout == nil {
			slog.WarnContext(ctx, "cannot disable plugin after revocation: no rollout controller", "plugin", k)
			continue
		}
		if _, err := s.d.Rollout.Disable(core.WithEmergencyRevocation(ctx), k, actorID, reason); err != nil {
			slog.ErrorContext(ctx, "disable plugin after revocation", "plugin", k, "err", err)
			continue
		}
		disabled = append(disabled, k)
	}
	return disabled
}

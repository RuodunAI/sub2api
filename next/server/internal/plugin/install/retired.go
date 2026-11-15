package install

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// A plugin key stays with its publisher after an uninstall (audit 2026-10-09
// P1-4): the plugin's accounts and their credentials, its plg_<key> schema
// and its KV entries can outlive the plugin, and whoever installs the key
// next reads them. Uninstall records the key in plugin_key_retirements; only
// the same publisher may install it again until an administrator releases
// it (ReleaseRetiredKey), optionally deleting what was left behind.

// ErrKeyRetired: the key belonged to another publisher's uninstalled plugin.
var ErrKeyRetired = core.NewError(http.StatusConflict, "plugin_key_retired",
	"the plugin key belonged to an uninstalled plugin of another publisher")

// PluginKVPurger deletes the KV entries of a plugin key.
type PluginKVPurger interface {
	PurgePluginKV(ctx context.Context, pluginKey string) (int, error)
}

// retireKey records the plugin's key and publisher; the caller holds the
// plugin row in tx and deletes it afterwards.
func retireKey(ctx context.Context, tx pgx.Tx, key string, actorID int64) error {
	return retire(store.G(ctx, tx), key, actorID).Error
}

// retire is the statement of retireKey.
func retire(db *gorm.DB, key string, actorID int64) *gorm.DB {
	return db.Exec(`INSERT INTO plugin_key_retirements (plugin_key, publisher_id, retired_by)
		SELECT key, publisher_id, ? FROM plugins WHERE key = ?
		ON CONFLICT (plugin_key) DO UPDATE SET publisher_id = EXCLUDED.publisher_id, retired_at = now(), retired_by = EXCLUDED.retired_by`,
		nullID(actorID), key)
}

// retirement is the plugin_key_retirements row of key.
func retirement(db *gorm.DB, key string) *gorm.DB {
	return db.Model(&model.PluginKeyRetirement{}).Where("plugin_key = ?", key)
}

// claimRetiredKey lets a new install of key by publisher proceed when the key
// was never retired or was retired by the same publisher. A built-in plugin
// takes its key back: the image decides (the reservation list keeps other
// publishers off first-party keys).
func claimRetiredKey(ctx context.Context, tx pgx.Tx, key string, publisher *int64, source string) error {
	g := store.G(ctx, tx)
	var r model.PluginKeyRetirement
	err := retirement(g, key).Select("publisher_id").Clauses(store.ForUpdate).Take(&r).Error
	switch {
	case store.IsNoRows(err):
		return nil
	case err != nil:
		return err
	case sameID(r.PublisherID, publisher):
		return nil
	case source == "builtin":
		slog.WarnContext(ctx, "builtin plugin takes back a key retired by another publisher", "plugin", key)
		return retirement(g, key).Delete(&model.PluginKeyRetirement{}).Error
	}
	return ErrKeyRetired.WithMessage(fmt.Sprintf("plugin key %q belonged to an uninstalled plugin of another publisher; "+
		"an administrator must release it first (POST /api/v1/plugins/retired-keys/%s/release), which can delete the accounts and data it left behind", key, key))
}

// RetiredKey is a plugin_key_retirements row with what the plugin left.
type RetiredKey struct {
	Key       string    `json:"key"`
	Publisher string    `json:"publisher"`
	RetiredAt time.Time `json:"retired_at"`
	// Installed: the same publisher installed the key again.
	Installed bool `json:"installed"`
	// Accounts not deleted that still belong to the key.
	Accounts int `json:"accounts"`
	// Schema: the plg_<key> schema (dbschema.SchemaName) still exists.
	Schema bool `json:"schema"`
}

// ListRetiredKeys lists the retired keys.
func (s *Service) ListRetiredKeys(ctx context.Context) ([]RetiredKey, error) {
	var rows []retiredKeyRow
	if err := retiredKeys(store.G(ctx, s.d.DB.Pool)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RetiredKey, len(rows))
	for i, r := range rows {
		out[i] = RetiredKey(r)
	}
	return out, nil
}

// retiredKeyRow is a row of retiredKeys.
type retiredKeyRow struct {
	Key       string    `gorm:"column:key"`
	Publisher string    `gorm:"column:publisher"`
	RetiredAt time.Time `gorm:"column:retired_at"`
	Installed bool      `gorm:"column:installed"`
	Accounts  int       `gorm:"column:accounts"`
	Schema    bool      `gorm:"column:schema"`
}

// retiredKeys selects the retired keys with what each plugin left, newest
// first.
func retiredKeys(db *gorm.DB) *gorm.DB {
	return db.Raw(`
		SELECT r.plugin_key AS key, COALESCE(pub.name, '') AS publisher, r.retired_at,
		  EXISTS (SELECT 1 FROM plugins p WHERE p.key = r.plugin_key) AS installed,
		  (SELECT count(*) FROM accounts a WHERE a.plugin_key = r.plugin_key AND a.deleted_at IS NULL) AS accounts,
		  EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = 'plg_' || r.plugin_key) AS schema
		FROM plugin_key_retirements r LEFT JOIN publishers pub ON pub.id = r.publisher_id
		ORDER BY r.retired_at DESC`)
}

// ReleaseOptions select what a release deletes besides the retirement.
type ReleaseOptions struct {
	// PurgeAccounts deletes the key's remaining accounts.
	PurgeAccounts bool `json:"purge_accounts"`
	// Purge drops the plg_<key> schema and deletes the key's KV entries.
	Purge bool `json:"purge"`
}

// ReleaseResult reports what a release deleted.
type ReleaseResult struct {
	AccountsDeleted int `json:"accounts_deleted"`
	KVDeleted       int `json:"kv_deleted"`
}

// ReleaseRetiredKey lets any publisher install key again. What is not purged
// is inherited by the next plugin with the key: the administrator decides.
func (s *Service) ReleaseRetiredKey(ctx context.Context, key string, opt ReleaseOptions, actorID int64) (ReleaseResult, error) {
	var res ReleaseResult
	if opt.PurgeAccounts && s.d.Accounts == nil {
		return res, core.ErrUnavailable.WithMessage("account purging is unavailable on this node")
	}
	ctx, release, guardErr := core.BeginPluginMutation(ctx, s.d.Mutations)
	if guardErr != nil {
		return res, guardErr
	}
	defer release()
	err := s.d.DB.Tx(ctx, func(tx pgx.Tx) error {
		g := store.G(ctx, tx)
		var one int
		err := retirement(g, key).Select("1").Clauses(store.ForUpdate).Take(&one).Error
		if store.IsNoRows(err) {
			return core.ErrNotFound.WithMessage("the plugin key is not retired")
		}
		if err != nil {
			return err
		}
		var installed bool
		if err := store.ExistsQuery(g, pluginRow(g, key)).Scan(&installed).Error; err != nil {
			return err
		}
		if installed {
			return core.ErrConflict.WithMessage("the plugin key is installed again; uninstall it first")
		}
		if opt.PurgeAccounts {
			n, err := s.d.Accounts.PurgePluginAccounts(ctx, key)
			res.AccountsDeleted = n
			if err != nil {
				return err
			}
		}
		if opt.Purge && s.d.Schemas != nil {
			if err := s.d.Schemas.DropTx(ctx, tx, key); err != nil {
				return fmt.Errorf("drop plugin schema: %w", err)
			}
		}
		if err := retirement(g, key).Delete(&model.PluginKeyRetirement{}).Error; err != nil {
			return err
		}
		return audit.Audit(ctx, tx, actorID, "plugin.key.release", "plugin", key, map[string]any{
			"purge": opt.Purge, "purge_accounts": opt.PurgeAccounts, "accounts_deleted": res.AccountsDeleted})
	})
	if err != nil {
		return res, err
	}
	if opt.Purge {
		res.KVDeleted = s.purgeKV(ctx, key)
	}
	return res, nil
}

// purgeKV deletes the key's KV entries, best effort: they are not part of
// the transaction, and a failure is logged.
func (s *Service) purgeKV(ctx context.Context, key string) int {
	if s.d.KV == nil {
		return 0
	}
	n, err := s.d.KV.PurgePluginKV(context.WithoutCancel(ctx), key)
	if err != nil {
		slog.WarnContext(ctx, "purge plugin KV entries", "plugin", key, "err", err)
	}
	return n
}

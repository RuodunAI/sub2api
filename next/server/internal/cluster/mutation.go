package cluster

import (
	"context"
	"time"

	"gorm.io/gorm"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

type PluginMutations struct {
	DB     *store.DB
	Locker core.Locker
	// AllowBootstrap is evaluated together with a host-only context marker.
	AllowBootstrap func() bool
}
type mutationScopeKey struct{}

func (g *PluginMutations) Begin(ctx context.Context) (context.Context, func(), error) {
	noop := func() {}
	if ctx.Value(mutationScopeKey{}) == g {
		return ctx, noop, ctx.Err()
	}
	if core.IsPluginBootstrap(ctx) && g.AllowBootstrap != nil && g.AllowBootstrap() {
		return ctx, noop, nil
	}
	if g.Locker == nil {
		return ctx, noop, core.ErrUnavailable.WithMessage("cluster change lock unavailable")
	}
	work, cancel := context.WithTimeout(ctx, 25*time.Second)
	lk, ok, err := g.Locker.TryLock(work, rc.ClusterChangeLock, 30*time.Second)
	if err != nil || !ok {
		cancel()
		if err != nil {
			return ctx, noop, err
		}
		return ctx, noop, core.ErrConflict.WithMessage("another cluster change is being submitted; retry")
	}
	work, stop := core.KeepLock(work, lk)
	done := func() { stop(); cancel(); lk.Release() }
	busy, err := CoreUpdateBusy(work, g.DB.Pool)
	if err == nil && busy {
		err = core.ErrConflict.WithMessage("core update is running or paused; finish or cancel it before changing plugins")
	}
	if err != nil {
		done()
		return ctx, noop, err
	}
	return context.WithValue(work, mutationScopeKey{}, g), done, nil
}

// CoreUpdateBusy also works on installations without a managed shell.
// Callers must fail closed on errors. The mutation gate still rechecks under
// the shared lock: this preflight alone does not authorize a plugin change.
func CoreUpdateBusy(ctx context.Context, q store.Querier) (bool, error) {
	db := store.G(ctx, q)
	var installed, busy bool
	if err := updaterInstalled(db, &installed).Error; err != nil || !installed {
		return false, err
	}
	err := upgradeActive(db, &busy).Error
	return busy, err
}

// updaterInstalled reads into installed whether the managed shell's
// updater schema is there; the table is not part of the core schema.
func updaterInstalled(db *gorm.DB, installed *bool) *gorm.DB {
	return db.Raw(`SELECT to_regclass('updater.upgrades') IS NOT NULL`).Scan(installed)
}

// upgradeActive reads into busy whether a core update is running or paused.
func upgradeActive(db *gorm.DB, busy *bool) *gorm.DB {
	return db.Raw(`SELECT EXISTS(SELECT 1 FROM updater.upgrades WHERE status IN ('running','paused'))`).Scan(busy)
}

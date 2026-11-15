package casbinx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// ResolverOption tunes NewResolver's engine.
type ResolverOption func(*EngineOptions)

// WithWait bounds how long a resolution waits for the snapshot rebuild
// (EngineOptions.Wait).
func WithWait(d time.Duration) ResolverOption { return func(o *EngineOptions) { o.Wait = d } }

// WithDebounce spaces snapshot rebuilds (EngineOptions.Debounce).
func WithDebounce(d time.Duration) ResolverOption {
	return func(o *EngineOptions) { o.Debounce = d }
}

// resolver is the Resolver: an Engine of snapshots built from l, whose role
// manager reads rc.
type resolver struct {
	eng *Engine
	rc  RoleCache
}

// NewResolver returns the Resolver over l and rc; self lists the self
// permission keys (authz.SelfPermissionKeys).
func NewResolver(l Loader, rc RoleCache, self []string, opts ...ResolverOption) Resolver {
	var eo EngineOptions
	for _, o := range opts {
		o(&eo)
	}
	return &resolver{eng: NewEngine(l, Options{Roles: rc, Self: self}, eo), rc: rc}
}

// Grants is uid's effective set at version min or newer. Its Version is the
// lower of the snapshot's and the roles' (both >= min).
func (r *resolver) Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	var set core.PermissionSet
	err := r.resolve(ctx, uid, min, func(s *Snapshot, roles int64) (err error) {
		if set, err = s.Grants(uid); err == nil {
			set.Version = lower(s.Version(), roles)
		}
		return err
	})
	return set, err
}

// Enforce is the matcher's answer for key, at version min or newer.
func (r *resolver) Enforce(ctx context.Context, uid int64, key string, min int64) (bool, error) {
	var ok bool
	err := r.resolve(ctx, uid, min, func(s *Snapshot, _ int64) (err error) {
		ok, err = s.Enforce(uid, key)
		return err
	})
	return ok, err
}

// resolve ensures the snapshot, warms uid's roles and runs fn on them. The
// role cache may evict uid between Warm and fn (ErrNotWarm): fn then runs
// once more after another Warm. Roles read below min fail the resolution
// (closed): min is committed, so no read should see an older version.
func (r *resolver) resolve(ctx context.Context, uid, min int64, fn func(s *Snapshot, roles int64) error) error {
	s, err := r.eng.Ensure(ctx, min)
	if err != nil {
		return err
	}
	for try := 0; ; try++ {
		ur, err := r.rc.Warm(ctx, uid, min)
		if err != nil {
			return err
		}
		if ur.Version < min {
			return fmt.Errorf("%w: roles of user %d read at version %d, below %d", ErrStale, uid, ur.Version, min)
		}
		err = fn(s, ur.Version)
		if try > 0 || !errors.Is(err, ErrNotWarm) {
			return err
		}
	}
}

func lower(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

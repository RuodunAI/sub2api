package casbinx

import (
	"context"
	"fmt"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// resolver is the Resolver: snapshots built from l, and the users' roles
// from rc.
type resolver struct {
	snaps *snapshots
	rc    RoleCache
}

// NewResolver returns the Resolver over l and rc; self lists the self
// permission keys (authz.SelfPermissionKeys).
func NewResolver(l Loader, rc RoleCache, self []string) Resolver {
	return newResolver(l, rc, self, snapshotOptions{})
}

func newResolver(l Loader, rc RoleCache, self []string, o snapshotOptions) *resolver {
	return &resolver{snaps: newSnapshots(func(ctx context.Context) (*snapshot, error) {
		p, err := l.Policy(ctx)
		if err != nil {
			return nil, err
		}
		return build(p, self)
	}, o), rc: rc}
}

// Grants is uid's effective set at version min or newer; its Version is the
// one both the policy and the roles were read at.
func (r *resolver) Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	s, ur, err := r.resolve(ctx, uid, min)
	if err != nil {
		return core.PermissionSet{}, err
	}
	return s.grants(ur)
}

func (r *resolver) enforce(ctx context.Context, uid int64, key string, min int64) (bool, int64, error) {
	s, ur, err := r.resolve(ctx, uid, min)
	if err != nil {
		return false, 0, err
	}
	ok, err := s.enforce(ur, key)
	return ok, s.version, err
}

func (r *resolver) Prewarm(min int64) { r.snaps.prewarm(min) }

// resolve returns a snapshot and uid's roles read at one version, min or
// newer. The two reads may land on different versions: a write committed
// between them, or the role cache holds a read newer than the snapshot.
// Combining them would make a set no version ever held (a role revoked and
// granted something new in one write: the old link with the new grant), so
// both are read once more at the newer of the two; still apart, ErrSkew.
func (r *resolver) resolve(ctx context.Context, uid, min int64) (*snapshot, UserRoles, error) {
	s, ur, err := r.read(ctx, uid, min)
	if err != nil || ur.Version == s.version {
		return s, ur, err
	}
	s, ur, err = r.read(ctx, uid, max(s.version, ur.Version))
	if err != nil || ur.Version == s.version {
		return s, ur, err
	}
	return nil, UserRoles{}, fmt.Errorf("%w: user %d: policy at version %d, roles at %d", ErrSkew, uid, s.version, ur.Version)
}

// read ensures a snapshot of version min or newer and warms uid's roles at
// its version.
func (r *resolver) read(ctx context.Context, uid, min int64) (*snapshot, UserRoles, error) {
	s, err := r.snaps.Ensure(ctx, min)
	if err != nil {
		return nil, UserRoles{}, err
	}
	ur, err := r.rc.Warm(ctx, uid, s.version)
	return s, ur, err
}

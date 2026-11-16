package casbinx

import (
	"context"
	"fmt"
	"time"

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

// Pauses between two reads of a resolution whose halves met different
// versions (resolve): the first retry is at once, the next ones back off.
const (
	skewPauseFirst = 2 * time.Millisecond
	skewPauseMax   = 100 * time.Millisecond
)

func nextSkewPause(p time.Duration) time.Duration {
	return max(skewPauseFirst, min(2*p, skewPauseMax))
}

// resolve returns a snapshot and uid's roles read at one version, min or
// newer. The two reads may land on different versions: a write committed
// between them, or the role cache holds a read newer than the snapshot.
// Combining them would make a set no version ever held (a role revoked and
// granted something new in one write: the old link with the new grant), so
// both are read again at the newer of the two, with a growing pause between
// rounds, until they meet: under a burst of writes (a bulk import bumps the
// version at each user) a round lands between two of them sooner or later.
// Only past the snapshot wait (snapshotOptions.Wait), or ctx's end, does it
// give up, with ErrSkew: a set it cannot read at one version is refused
// (fail closed), as a snapshot it cannot have in time is.
func (r *resolver) resolve(ctx context.Context, uid, min int64) (*snapshot, UserRoles, error) {
	deadline := time.Now().Add(r.snaps.o.Wait)
	s, ur, err := r.read(ctx, uid, min)
	if err != nil || ur.Version == s.version {
		return s, ur, err
	}
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for pause := time.Duration(0); ; pause = nextSkewPause(pause) {
		skew := fmt.Errorf("%w: user %d: policy at version %d, roles at %d", ErrSkew, uid, s.version, ur.Version)
		min = max(s.version, ur.Version)
		select {
		case <-time.After(pause):
		case <-wctx.Done():
			if ctx.Err() != nil {
				return nil, UserRoles{}, ctx.Err()
			}
			return nil, UserRoles{}, skew
		}
		s, ur, err = r.read(wctx, uid, min)
		switch {
		case err != nil && wctx.Err() != nil && ctx.Err() == nil:
			return nil, UserRoles{}, skew // the wait ran out while skewed
		case err != nil:
			return nil, UserRoles{}, err
		case ur.Version == s.version:
			return s, ur, nil
		}
	}
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

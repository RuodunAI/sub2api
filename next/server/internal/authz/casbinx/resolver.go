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
// one the roles were read at, under the snapshot's policy version.
func (r *resolver) Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	s, ur, err := r.resolve(ctx, uid, min)
	if err != nil {
		return core.PermissionSet{}, err
	}
	return s.grants(ur)
}

func (r *resolver) Prewarm(policy int64) { r.snaps.prewarm(policy) }

// Pauses between two reads of a resolution whose halves met different
// policy versions (resolve): the first retry is at once, the next ones
// back off.
const (
	skewPauseFirst = 2 * time.Millisecond
	skewPauseMax   = 100 * time.Millisecond
)

func nextSkewPause(p time.Duration) time.Duration {
	return max(skewPauseFirst, min(2*p, skewPauseMax))
}

// resolve returns uid's roles read at version min or newer and the
// snapshot of the policy version they were read under. The roles come
// first, then the snapshot of their policy version (the current one, or one
// built for it). A snapshot newer than the roles' policy (a policy write
// committed between the two reads, or a rebuild that read later) means the
// roles were read before it: they are read again at once, at the
// snapshot's authz version or newer, which matches unless yet another
// policy write landed in between; then, after a growing pause, the round
// starts over. Combining a snapshot and roles of different policy versions
// would make a set no version ever held (a role revoked and granted
// something new in one write: the old link with the new grant). Only
// policy writes move the policy version, so a round rarely misses. Past
// the snapshot wait (snapshotOptions.Wait), or ctx's end, it gives up with
// ErrSkew: a set it cannot read under one policy is refused (fail closed),
// as a snapshot it cannot have in time is.
func (r *resolver) resolve(ctx context.Context, uid, min int64) (*snapshot, UserRoles, error) {
	wctx, cancel := context.WithTimeout(ctx, r.snaps.o.Wait)
	defer cancel()
	// The first read runs on ctx: a snapshot wait ending is ErrStale's.
	ur, err := r.rc.Warm(ctx, uid, min)
	if err != nil {
		return nil, UserRoles{}, err
	}
	for pause := time.Duration(0); ; pause = nextSkewPause(pause) {
		s, err := r.snaps.Ensure(ctx, ur.PolicyVersion)
		if err != nil || s.policy == ur.PolicyVersion {
			return s, ur, err
		}
		skew := fmt.Errorf("%w: user %d: policy snapshot at %d, roles read under %d", ErrSkew, uid, s.policy, ur.PolicyVersion)
		if ur, err = r.rc.Warm(wctx, uid, max(min, s.version)); err == nil && ur.PolicyVersion == s.policy {
			return s, ur, nil
		}
		select {
		case <-time.After(pause):
		case <-wctx.Done():
		}
		switch {
		case ctx.Err() != nil:
			return nil, UserRoles{}, ctx.Err()
		case wctx.Err() != nil:
			return nil, UserRoles{}, skew // the wait ran out while skewed
		case err != nil:
			return nil, UserRoles{}, err
		}
	}
}

package casbinx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// roleCacheOptions tunes newRoleCache (tests shorten them); the zero value
// is the default.
type roleCacheOptions struct {
	// TTL is the lifetime of a Valkey entry (default 15 min). The version
	// check decides whether an entry is usable; the TTL only bounds memory.
	TTL time.Duration
	// L2Timeout bounds each Valkey call (default 100 ms): L2 only saves a
	// PostgreSQL read, so waiting longer than that read takes is a loss.
	L2Timeout time.Duration
	// L2Backoff is how long L2 is skipped after a Valkey error (default
	// 10 s), so a hung Valkey costs one timeout per period instead of one
	// per cache miss.
	L2Backoff time.Duration
}

func (o roleCacheOptions) withDefaults() roleCacheOptions {
	if o.TTL <= 0 {
		o.TTL = roleCacheTTL
	}
	if o.L2Timeout <= 0 {
		o.L2Timeout = roleCacheL2Timeout
	}
	if o.L2Backoff <= 0 {
		o.L2Backoff = roleCacheL2Backoff
	}
	return o
}

const (
	roleCacheTTL       = 15 * time.Minute
	roleCacheL2Timeout = 100 * time.Millisecond
	roleCacheL2Backoff = 10 * time.Second
	// roleCacheLoadTimeout bounds one shared load: it runs detached from
	// the request that started it, which may end first.
	roleCacheLoadTimeout = 5 * time.Second
	// roleCacheL2Writes bounds the L2 writes in flight: they run off the
	// request, and past the bound (a slow Valkey) one is dropped: L2 only
	// saves another node a PostgreSQL read.
	roleCacheL2Writes = 64
	// roleCacheKeyPrefix follows the "<domain>:<kind>:<id>" layout of the
	// other keys (account:balance:<id>, sticky:stats:...); c marks a cache
	// entry, ur the user's roles.
	roleCacheKeyPrefix = "authz:c:ur:"
	// roleCacheWarnEvery throttles the warnings of Valkey errors.
	roleCacheWarnEvery = time.Minute
)

// roleWire is the Valkey value: {"v":123,"p":45,"a":true,"r":["r:user"]}.
type roleWire struct {
	V int64    `json:"v"`
	P int64    `json:"p"`
	A bool     `json:"a"`
	R []string `json:"r"`
}

type roleCache struct {
	load Loader
	rdb  redis.UniversalClient
	o    roleCacheOptions

	flight   singleflight.Group
	l2Until  atomic.Int64 // unix nanos before which L2 is skipped
	warnedAt atomic.Int64 // unix nanos of the last Valkey warning
	// l2Writes holds a slot per L2 write in flight; writing counts them
	// (tests wait for them).
	l2Writes chan struct{}
	writing  sync.WaitGroup
}

var _ RoleCache = (*roleCache)(nil)

// NewRoleCache returns the user-role cache over l. rdb may be nil: every
// read then goes to l.
func NewRoleCache(l Loader, rdb redis.UniversalClient) RoleCache {
	return newRoleCache(l, rdb, roleCacheOptions{})
}

func newRoleCache(l Loader, rdb redis.UniversalClient, o roleCacheOptions) *roleCache {
	return &roleCache{load: l, rdb: rdb, o: o.withDefaults(), l2Writes: make(chan struct{}, roleCacheL2Writes)}
}

// Warm returns uid's roles read at a version >= min. A read that is still
// older (the Loader saw an older snapshot) is returned as is, for the
// caller to judge by its Version.
func (c *roleCache) Warm(ctx context.Context, uid, min int64) (UserRoles, error) {
	return c.shared(ctx, uid, min)
}

// shared merges the concurrent loads of one user at one min: a load for a
// lower min may take an L2 entry this caller cannot use. The load is
// detached from ctx (another caller may still wait for it); ctx only bounds
// this wait.
func (c *roleCache) shared(ctx context.Context, uid, min int64) (UserRoles, error) {
	base := context.WithoutCancel(ctx)
	ch := c.flight.DoChan(strconv.FormatInt(uid, 10)+":"+strconv.FormatInt(min, 10), func() (any, error) {
		lctx, cancel := context.WithTimeout(base, roleCacheLoadTimeout)
		defer cancel()
		return c.fill(lctx, uid, min)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return UserRoles{}, r.Err
		}
		return r.Val.(UserRoles), nil
	case <-ctx.Done():
		return UserRoles{}, ctx.Err()
	}
}

// fill reads uid's roles from L2 or, when L2 has none at version min, from
// the Loader, writing them back to L2 off the caller.
func (c *roleCache) fill(ctx context.Context, uid, min int64) (UserRoles, error) {
	if ur, ok := c.getL2(ctx, uid, min); ok {
		return ur, nil
	}
	ur, err := c.load.UserRoles(ctx, uid)
	if err != nil {
		return UserRoles{}, err
	}
	c.setL2(uid, ur)
	return ur, nil
}

func roleCacheKey(uid int64) string { return roleCacheKeyPrefix + strconv.FormatInt(uid, 10) }

// l2 reports whether L2 is configured and not backing off after an error.
func (c *roleCache) l2() bool {
	return c.rdb != nil && time.Now().UnixNano() >= c.l2Until.Load()
}

// getL2 returns uid's Valkey entry when it is at exactly version min. Any
// failure is a miss: PostgreSQL holds the truth.
//
// Not a newer one: a database restored from a backup goes back to an older
// authz version, and the entries written before the restore are newer than
// anything it holds, so "at least min" would answer from before the
// restore at once. An exact match narrows that without closing it: a
// version number names one state within one history of the database only,
// and once the restored database's own writes bring it back to the version
// of an entry written before the restore (within the TTL), that entry
// names another state. Restoring a backup therefore deletes the keys
// (CONTRACTS §99.4). The resolver asks Warm at its snapshot's version, so the
// entries a node can use are those of the current version, the ones the
// other nodes write.
func (c *roleCache) getL2(ctx context.Context, uid, min int64) (UserRoles, bool) {
	if !c.l2() {
		return UserRoles{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, c.o.L2Timeout)
	defer cancel()
	raw, err := c.rdb.Get(ctx, roleCacheKey(uid)).Bytes()
	if errors.Is(err, redis.Nil) {
		return UserRoles{}, false
	}
	if err != nil {
		c.l2Failed(ctx, "read", uid, err)
		return UserRoles{}, false
	}
	var w roleWire
	if err := json.Unmarshal(raw, &w); err != nil || w.V != min {
		return UserRoles{}, false
	}
	return UserRoles{Version: w.V, PolicyVersion: w.P, Active: w.A, Roles: w.R}, true
}

// setL2 shares a PostgreSQL read with the other nodes, off the caller and
// best effort: a write waiting for a free slot is dropped. It replaces any
// entry: with exact version matches (getL2), keeping a newer one would
// gain nothing and would keep the entries of a restored database's past
// for their whole TTL. An older node's write over a newer entry costs the
// newer version's readers a miss, never a wrong answer.
func (c *roleCache) setL2(uid int64, ur UserRoles) {
	if !c.l2() {
		return
	}
	select {
	case c.l2Writes <- struct{}{}:
	default:
		return
	}
	w := roleWire{V: ur.Version, P: ur.PolicyVersion, A: ur.Active, R: ur.Roles}
	if !w.A || w.R == nil {
		w.R = []string{} // an inactive user holds nothing; "r":[] not null
	}
	c.writing.Add(1)
	go func() {
		defer c.writing.Done()
		defer func() { <-c.l2Writes }()
		raw, err := json.Marshal(w)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.o.L2Timeout)
		defer cancel()
		if err := c.rdb.Set(ctx, roleCacheKey(uid), raw, c.o.TTL).Err(); err != nil {
			c.l2Failed(ctx, "write", uid, err)
		}
	}()
}

// l2Failed backs off L2 and warns at most once per roleCacheWarnEvery.
func (c *roleCache) l2Failed(ctx context.Context, op string, uid int64, err error) {
	now := time.Now().UnixNano()
	c.l2Until.Store(now + int64(c.o.L2Backoff))
	last := c.warnedAt.Load()
	if now-last < int64(roleCacheWarnEvery) || !c.warnedAt.CompareAndSwap(last, now) {
		return
	}
	slog.WarnContext(ctx, "authz: valkey role cache unavailable, reading PostgreSQL", "op", op, "user", uid, "backoff", c.o.L2Backoff, "err", err)
}

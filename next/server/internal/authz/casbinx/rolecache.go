package casbinx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// RoleCacheOptions tunes NewRoleCache; the zero value is the default.
type RoleCacheOptions struct {
	// TTL is the lifetime of a Valkey entry (default 15 min). The version
	// check decides whether an entry is usable; the TTL only bounds memory.
	TTL time.Duration
	// Capacity bounds the users held in the process (default 200 000,
	// about 100 B each).
	Capacity int
	// L2Timeout bounds each Valkey call (default 100 ms): L2 only saves a
	// PostgreSQL read, so waiting longer than that read takes is a loss.
	L2Timeout time.Duration
	// L2Backoff is how long L2 is skipped after a Valkey error (default
	// 10 s), so a hung Valkey costs one timeout per period instead of one
	// per cache miss.
	L2Backoff time.Duration
}

const (
	roleCacheTTL       = 15 * time.Minute
	roleCacheCapacity  = 200_000
	roleCacheL2Timeout = 100 * time.Millisecond
	roleCacheL2Backoff = 10 * time.Second
	// roleCacheLoadTimeout bounds one shared load: it runs detached from
	// the request that started it, which may end first.
	roleCacheLoadTimeout = 5 * time.Second
	// roleCacheKeyPrefix follows the "<domain>:<kind>:<id>" layout of the
	// other keys (account:balance:<id>, sticky:stats:...); c marks a cache
	// entry, ur the user's roles.
	roleCacheKeyPrefix = "authz:c:ur:"
)

// roleCacheWarnEvery throttles the warnings of Valkey errors; a variable so
// tests can shorten it.
var roleCacheWarnEvery = time.Minute

// roleCacheSet writes a role entry unless Valkey holds one of a newer
// version: two nodes loading the same user at different versions must not
// leave the older read behind. An unreadable current value is overwritten.
var roleCacheSet = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local ok, d = pcall(cjson.decode, cur)
  if ok and type(d) == 'table' and tonumber(d.v) and tonumber(d.v) > tonumber(ARGV[1]) then
    return 0
  end
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1`)

// roleEntry is one user's roles at version v. It is immutable once stored
// except for used, the cache's clock at its last Warm, which picks the
// victims of a full cache.
type roleEntry struct {
	v      int64
	active bool
	roles  []string // the g rows as read, without SelfRole
	sub    []string // what Roles answers: roles + SelfRole, nil when inactive
	used   atomic.Uint64
}

func newRoleEntry(ur UserRoles) *roleEntry {
	e := &roleEntry{v: ur.Version, active: ur.Active}
	if ur.Active {
		e.roles = append([]string(nil), ur.Roles...)
		e.sub = append(append(make([]string, 0, len(ur.Roles)+1), ur.Roles...), SelfRole)
	}
	return e
}

func (e *roleEntry) userRoles() UserRoles {
	return UserRoles{Version: e.v, Active: e.active, Roles: append([]string(nil), e.roles...)}
}

// roleWire is the Valkey value: {"v":123,"a":true,"r":["r:user"]}.
type roleWire struct {
	V int64    `json:"v"`
	A bool     `json:"a"`
	R []string `json:"r"`
}

type roleCache struct {
	load Loader
	rdb  redis.UniversalClient
	o    RoleCacheOptions

	mu    sync.RWMutex
	users map[int64]*roleEntry
	// newest is the highest version stored, clock the counter behind
	// roleEntry.used; both guide eviction.
	newest int64
	clock  atomic.Uint64

	flight   singleflight.Group
	l2Until  atomic.Int64 // unix nanos before which L2 is skipped
	warnedAt atomic.Int64 // unix nanos of the last Valkey warning
}

var _ RoleCache = (*roleCache)(nil)

// NewRoleCache returns the two-level user-role cache over l. rdb may be nil:
// the cache then has the process level only.
func NewRoleCache(l Loader, rdb redis.UniversalClient, o RoleCacheOptions) RoleCache {
	if o.TTL <= 0 {
		o.TTL = roleCacheTTL
	}
	if o.Capacity <= 0 {
		o.Capacity = roleCacheCapacity
	}
	if o.L2Timeout <= 0 {
		o.L2Timeout = roleCacheL2Timeout
	}
	if o.L2Backoff <= 0 {
		o.L2Backoff = roleCacheL2Backoff
	}
	return &roleCache{load: l, rdb: rdb, o: o, users: map[int64]*roleEntry{}}
}

// Roles answers g for a user subject from the process level only: the
// user's roles plus SelfRole when active, none (but ok) when not. Other
// subjects and users not warmed are not held (fail closed).
func (c *roleCache) Roles(sub string) ([]string, bool) {
	uid, ok := parseUserSub(sub)
	if !ok {
		return nil, false
	}
	c.mu.RLock()
	e := c.users[uid]
	c.mu.RUnlock()
	if e == nil {
		return nil, false
	}
	// A copy: the caller (Casbin) may append to it.
	return append([]string(nil), e.sub...), true
}

// parseUserSub is the inverse of UserSub; only its exact output is a user.
func parseUserSub(sub string) (int64, bool) {
	s, ok := strings.CutPrefix(sub, "u:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && UserSub(id) == sub
}

// Warm makes the process hold uid's roles read at a version >= min. A read
// that is still older (the Loader saw an older snapshot) is returned as is,
// for the caller to judge by its Version.
func (c *roleCache) Warm(ctx context.Context, uid, min int64) (UserRoles, error) {
	if ur, ok := c.fresh(uid, min); ok {
		return ur, nil
	}
	ur, shared, err := c.shared(ctx, uid, min)
	if err != nil || !shared || ur.Version >= min {
		return ur, err
	}
	// The load this call joined was started for a lower min and may have
	// read before min was committed: one more, of its own.
	if ur, ok := c.fresh(uid, min); ok {
		return ur, nil
	}
	ur, _, err = c.shared(ctx, uid, min)
	return ur, err
}

// fresh returns uid's process entry when it is at a version >= min.
func (c *roleCache) fresh(uid, min int64) (UserRoles, bool) {
	c.mu.RLock()
	e := c.users[uid]
	c.mu.RUnlock()
	if e == nil || e.v < min {
		return UserRoles{}, false
	}
	e.used.Store(c.clock.Add(1))
	return e.userRoles(), true
}

// shared merges the concurrent loads of one user. The load is detached from
// ctx (another caller may still wait for it); ctx only bounds this wait.
func (c *roleCache) shared(ctx context.Context, uid, min int64) (UserRoles, bool, error) {
	base := context.WithoutCancel(ctx)
	ch := c.flight.DoChan(strconv.FormatInt(uid, 10), func() (any, error) {
		lctx, cancel := context.WithTimeout(base, roleCacheLoadTimeout)
		defer cancel()
		return c.fill(lctx, uid, min)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return UserRoles{}, r.Shared, r.Err
		}
		return r.Val.(UserRoles), r.Shared, nil
	case <-ctx.Done():
		return UserRoles{}, false, ctx.Err()
	}
}

// fill reads uid's roles from L2 or, when L2 has none at a version >= min,
// from the Loader (writing them back to L2), and stores them in L1.
func (c *roleCache) fill(ctx context.Context, uid, min int64) (UserRoles, error) {
	ur, l2ok := c.getL2(ctx, uid, min)
	if !l2ok {
		var err error
		if ur, err = c.load.UserRoles(ctx, uid); err != nil {
			return UserRoles{}, err
		}
		c.setL2(ctx, uid, ur)
	}
	return c.store(uid, ur), nil
}

// store keeps ur in L1 unless L1 already holds a newer read, and returns
// what L1 holds for uid afterwards.
func (c *roleCache) store(uid int64, ur UserRoles) UserRoles {
	e := newRoleEntry(ur)
	e.used.Store(c.clock.Add(1))
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.users[uid]; cur != nil && cur.v > e.v {
		return cur.userRoles()
	}
	if _, ok := c.users[uid]; !ok && len(c.users) >= c.o.Capacity {
		c.evictLocked()
	}
	c.users[uid] = e
	c.newest = max(c.newest, e.v)
	return e.userRoles()
}

// evictLocked frees an eighth of the capacity in one sweep, so the O(n)
// sweep runs once per Capacity/8 new users. Entries of an older version go
// first: the authz version is global, so the next Warm reloads them anyway.
// When that is not enough (all current), the least recently warmed go: an
// entry just warmed is about to be read by the role manager, and dropping it
// there would resolve its user to nothing.
func (c *roleCache) evictLocked() {
	keep := c.o.Capacity - max(c.o.Capacity/8, 1)
	for uid, e := range c.users {
		if e.v < c.newest {
			delete(c.users, uid)
		}
	}
	if len(c.users) <= keep {
		return
	}
	// Each entry's used is a distinct tick, so at most keep entries were
	// warmed within the last keep ticks. Every entry took a tick, so the
	// clock is past keep here.
	cut := c.clock.Load() - uint64(keep)
	for uid, e := range c.users {
		if e.used.Load() <= cut {
			delete(c.users, uid)
		}
	}
}

func roleCacheKey(uid int64) string { return roleCacheKeyPrefix + strconv.FormatInt(uid, 10) }

// l2 reports whether L2 is configured and not backing off after an error.
func (c *roleCache) l2() bool {
	return c.rdb != nil && time.Now().UnixNano() >= c.l2Until.Load()
}

// getL2 returns uid's Valkey entry when it is at a version >= min. Any
// failure is a miss: PostgreSQL holds the truth.
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
	if err := json.Unmarshal(raw, &w); err != nil || w.V < min {
		return UserRoles{}, false
	}
	return UserRoles{Version: w.V, Active: w.A, Roles: w.R}, true
}

// setL2 shares a PostgreSQL read with the other nodes (best effort).
func (c *roleCache) setL2(ctx context.Context, uid int64, ur UserRoles) {
	if !c.l2() {
		return
	}
	w := roleWire{V: ur.Version, A: ur.Active, R: ur.Roles}
	if !w.A || w.R == nil {
		w.R = []string{} // an inactive user holds nothing; "r":[] not null
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, c.o.L2Timeout)
	defer cancel()
	err = roleCacheSet.Run(ctx, c.rdb, []string{roleCacheKey(uid)}, ur.Version, raw, c.o.TTL.Milliseconds()).Err()
	if err != nil {
		c.l2Failed(ctx, "write", uid, err)
	}
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

package casbinx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRoleLoader answers UserRoles from a function and counts the calls;
// gate, when set, holds every call until it is closed.
type fakeRoleLoader struct {
	calls atomic.Int64
	gate  chan struct{}
	in    chan struct{} // signalled when a call starts (optional)
	fn    func(uid int64, call int64) (UserRoles, error)
}

func (f *fakeRoleLoader) Policy(context.Context) (Policy, error) {
	return Policy{}, errors.New("not used")
}

func (f *fakeRoleLoader) UserRoles(ctx context.Context, uid int64) (UserRoles, error) {
	n := f.calls.Add(1)
	if f.in != nil {
		f.in <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	return f.fn(uid, n)
}

// at answers every user with roles at version v, active.
func at(v int64, roles ...string) func(int64, int64) (UserRoles, error) {
	return func(int64, int64) (UserRoles, error) {
		return UserRoles{Version: v, Active: true, Roles: roles}, nil
	}
}

func newTestRedis(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// readL2 is uid's Valkey entry once rc's writes in flight are done.
func readL2(t *testing.T, rc *roleCache, mr *miniredis.Miniredis, uid int64) roleWire {
	t.Helper()
	rc.writing.Wait()
	raw, err := mr.Get(roleCacheKey(uid))
	require.NoError(t, err)
	var w roleWire
	require.NoError(t, json.Unmarshal([]byte(raw), &w))
	return w
}

func TestRoleCacheOlderReadReturnedOnce(t *testing.T) {
	// The Loader still reads version 3 while min is 5: the read is returned
	// for the caller to judge, after one load, not retried in a loop.
	l := &fakeRoleLoader{fn: at(3, "r:user")}
	rc := newRoleCache(l, nil, roleCacheOptions{})
	ur, err := rc.Warm(context.Background(), 7, 5)
	require.NoError(t, err)
	require.EqualValues(t, 3, ur.Version)
	require.EqualValues(t, 1, l.calls.Load())
}

func TestRoleCacheL2Hit(t *testing.T) {
	mr, rdb := newTestRedis(t)
	require.NoError(t, mr.Set(roleCacheKey(7), `{"v":5,"a":true,"r":["r:user","r:supplier"]}`))
	l := &fakeRoleLoader{fn: at(5, "r:wrong")}
	rc := newRoleCache(l, rdb, roleCacheOptions{})

	ur, err := rc.Warm(context.Background(), 7, 5)
	require.NoError(t, err)
	require.Equal(t, UserRoles{Version: 5, Active: true, Roles: []string{"r:user", "r:supplier"}}, ur)
	require.Zero(t, l.calls.Load(), "L2 at version min spares the Loader")

	mr.FlushAll()
	_, err = rc.Warm(context.Background(), 7, 5)
	require.NoError(t, err)
	require.EqualValues(t, 1, l.calls.Load(), "no process level: L2 gone, the Loader reads")
}

// An L2 entry is taken at exactly min only: an older one misses a write,
// and a newer one may come from before a database restore took the
// version back (the restored state at 9 is not the one Valkey holds).
func TestRoleCacheL2OtherVersionIgnored(t *testing.T) {
	for _, stored := range []int64{3, 9} {
		mr, rdb := newTestRedis(t)
		require.NoError(t, mr.Set(roleCacheKey(7), fmt.Sprintf(`{"v":%d,"a":true,"r":["r:other"]}`, stored)))
		l := &fakeRoleLoader{fn: at(5, "r:user")}
		rc := newRoleCache(l, rdb, roleCacheOptions{TTL: time.Minute})

		ur, err := rc.Warm(context.Background(), 7, 5)
		require.NoError(t, err)
		require.Equal(t, []string{"r:user"}, ur.Roles, "stored at v%d", stored)
		require.EqualValues(t, 1, l.calls.Load())
		require.Equal(t, roleWire{V: 5, A: true, R: []string{"r:user"}}, readL2(t, rc, mr, 7), "the PostgreSQL read replaces v%d", stored)
		require.Equal(t, time.Minute, mr.TTL(roleCacheKey(7)))
	}
}

func TestRoleCacheValkeyDownFallsBackToLoader(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: at(5, "r:user")}
	rc := newRoleCache(l, rdb, roleCacheOptions{L2Timeout: 50 * time.Millisecond, L2Backoff: time.Hour})
	mr.Close()

	var logs atomic.Int64
	prev := slog.Default()
	slog.SetDefault(slog.New(countHandler{&logs}))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	for uid := int64(1); uid <= 3; uid++ {
		ur, err := rc.Warm(ctx, uid, 5)
		require.NoError(t, err)
		require.Equal(t, []string{"r:user"}, ur.Roles)
	}
	require.EqualValues(t, 3, l.calls.Load())
	require.EqualValues(t, 1, logs.Load(), "the warning is throttled")
	require.False(t, rc.l2(), "L2 backs off after an error")
}

func TestRoleCacheValkeyBackOnline(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: at(5, "r:user")}
	rc := newRoleCache(l, rdb, roleCacheOptions{L2Timeout: 50 * time.Millisecond, L2Backoff: time.Millisecond})
	addr := mr.Addr()
	mr.Close()
	_, err := rc.Warm(context.Background(), 1, 5)
	require.NoError(t, err)

	require.NoError(t, mr.StartAddr(addr))
	require.Eventually(t, rc.l2, time.Second, time.Millisecond)
	_, err = rc.Warm(context.Background(), 2, 5)
	require.NoError(t, err)
	require.EqualValues(t, 5, readL2(t, rc, mr, 2).V, "L2 is written again once the backoff ends")
}

// A write replaces whatever L2 holds, a newer version included: with exact
// matches a newer entry gains nothing, and keeping it would pin a restored
// database's past.
func TestRoleCacheL2WriteReplaces(t *testing.T) {
	mr, rdb := newTestRedis(t)
	rc := newRoleCache(&fakeRoleLoader{}, rdb, roleCacheOptions{})

	require.NoError(t, mr.Set(roleCacheKey(7), `{"v":9,"a":true,"r":["r:before_restore"]}`))
	rc.setL2(7, UserRoles{Version: 5, Active: true, Roles: []string{"r:restored"}})
	require.Equal(t, roleWire{V: 5, A: true, R: []string{"r:restored"}}, readL2(t, rc, mr, 7))

	require.NoError(t, mr.Set(roleCacheKey(7), `not json`))
	rc.setL2(7, UserRoles{Version: 6})
	require.Equal(t, roleWire{V: 6, R: []string{}}, readL2(t, rc, mr, 7), "an unreadable value is replaced")
}

// L2 writes run off the caller and are dropped, not queued, while every
// slot is taken (a slow Valkey).
func TestRoleCacheL2WritesBounded(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: at(5, "r:user")}
	rc := newRoleCache(l, rdb, roleCacheOptions{})
	for range cap(rc.l2Writes) {
		rc.l2Writes <- struct{}{}
	}
	_, err := rc.Warm(context.Background(), 7, 5)
	require.NoError(t, err)
	rc.writing.Wait()
	require.False(t, mr.Exists(roleCacheKey(7)), "dropped while full")

	for range cap(rc.l2Writes) {
		<-rc.l2Writes
	}
	_, err = rc.Warm(context.Background(), 8, 5)
	require.NoError(t, err)
	require.EqualValues(t, 5, readL2(t, rc, mr, 8).V)
}

func TestRoleCacheConcurrentWarmLoadsOnce(t *testing.T) {
	_, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: at(5, "r:user"), gate: make(chan struct{}), in: make(chan struct{}, 64)}
	rc := newRoleCache(l, rdb, roleCacheOptions{})

	const n = 32
	var wg sync.WaitGroup
	results := make([]UserRoles, n)
	for i := range n {
		wg.Go(func() {
			ur, err := rc.Warm(context.Background(), 7, 5)
			assert.NoError(t, err)
			results[i] = ur
		})
	}
	<-l.in
	time.Sleep(20 * time.Millisecond) // let the others join the flight
	close(l.gate)
	wg.Wait()
	require.EqualValues(t, 1, l.calls.Load())
	for _, ur := range results {
		require.EqualValues(t, 5, ur.Version)
	}
}

func TestRoleCacheLoadsMergedPerMin(t *testing.T) {
	// B (min 2) does not join the load A (min 1) started, which may take an
	// entry of version 1: it loads on its own.
	l := &fakeRoleLoader{fn: func(_ int64, n int64) (UserRoles, error) {
		return UserRoles{Version: n, Active: true}, nil
	}, gate: make(chan struct{}), in: make(chan struct{}, 4)}
	rc := newRoleCache(l, nil, roleCacheOptions{})

	var a, b UserRoles
	var wg sync.WaitGroup
	wg.Go(func() { a, _ = rc.Warm(context.Background(), 7, 1) })
	<-l.in
	wg.Go(func() { b, _ = rc.Warm(context.Background(), 7, 2) })
	time.Sleep(20 * time.Millisecond)
	close(l.gate)
	wg.Wait()
	require.EqualValues(t, 1, a.Version)
	require.EqualValues(t, 2, b.Version)
	require.EqualValues(t, 2, l.calls.Load())
}

func TestRoleCacheCallerCancelDoesNotAbortLoad(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: at(5, "r:user"), gate: make(chan struct{}), in: make(chan struct{}, 1)}
	rc := newRoleCache(l, rdb, roleCacheOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := rc.Warm(ctx, 7, 5); done <- err }()
	<-l.in
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(l.gate)
	// The load ran to its end: its read reached L2.
	require.Eventually(t, func() bool { return mr.Exists(roleCacheKey(7)) }, time.Second, time.Millisecond)
}

func TestRoleCacheLoaderErrorNotCached(t *testing.T) {
	boom := errors.New("pg down")
	l := &fakeRoleLoader{fn: func(int64, int64) (UserRoles, error) { return UserRoles{}, boom }}
	rc := newRoleCache(l, nil, roleCacheOptions{})
	_, err := rc.Warm(context.Background(), 7, 1)
	require.ErrorIs(t, err, boom)
	_, err = rc.Warm(context.Background(), 7, 1)
	require.ErrorIs(t, err, boom)
	require.EqualValues(t, 2, l.calls.Load(), "nothing cached")
}

func TestRoleCacheInactiveUser(t *testing.T) {
	mr, rdb := newTestRedis(t)
	l := &fakeRoleLoader{fn: func(int64, int64) (UserRoles, error) {
		return UserRoles{Version: 4, Active: false, Roles: []string{"r:admin"}}, nil
	}}
	rc := newRoleCache(l, rdb, roleCacheOptions{})

	ur, err := rc.Warm(context.Background(), 7, 4)
	require.NoError(t, err)
	require.False(t, ur.Active, "an inactive user holds nothing")
	rc.writing.Wait()
	raw, err := mr.Get(roleCacheKey(7))
	require.NoError(t, err)
	require.JSONEq(t, `{"v":4,"p":0,"a":false,"r":[]}`, raw)

	// Another node reads the inactive entry from L2.
	other := newRoleCache(l, rdb, roleCacheOptions{})
	ur, err = other.Warm(context.Background(), 7, 4)
	require.NoError(t, err)
	require.False(t, ur.Active)
	require.EqualValues(t, 1, l.calls.Load())
}

// countHandler counts warning records.
type countHandler struct{ n *atomic.Int64 }

func (h countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		h.n.Add(1)
	}
	return nil
}
func (h countHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countHandler) WithGroup(string) slog.Handler      { return h }

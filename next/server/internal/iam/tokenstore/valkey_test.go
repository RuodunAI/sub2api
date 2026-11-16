package tokenstore

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// clock is a settable time for both the store and miniredis.
type clock struct {
	mr  *miniredis.Miniredis
	now time.Time
}

func (c *clock) Now() time.Time { return c.now }

// advance moves both clocks: miniredis expires keys by FastForward.
func (c *clock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	c.mr.FastForward(d)
}

func newStore(t *testing.T) (*Valkey, *clock) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := &clock{mr: mr, now: time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)}
	// No retries: the outage test fails fast.
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewValkey(rdb, Options{Idle: 7 * 24 * time.Hour, Max: 30 * 24 * time.Hour, Now: c.Now}), c
}

var ctx = context.Background()

func create(t *testing.T, v *Valkey, uid int64, id string) string {
	t.Helper()
	tok, err := v.Create(ctx, Session{ID: id, UserID: uid, Method: "password", IP: "192.0.2.1", UserAgent: "ua", TokenVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func mustLookup(t *testing.T, v *Valkey, tok string) Session {
	t.Helper()
	s, err := v.Lookup(ctx, tok)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return s
}

func gone(t *testing.T, v *Valkey, tok string) {
	t.Helper()
	if _, err := v.Lookup(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup of an ended session: %v", err)
	}
}

func TestCreateLookup(t *testing.T) {
	v, c := newStore(t)
	tok := create(t, v, 7, "fam1")
	if !strings.HasPrefix(tok, SessionPrefix) || len(tok) != len(SessionPrefix)+43 {
		t.Fatalf("token %q", tok)
	}
	s := mustLookup(t, v, tok)
	if s.ID != "fam1" || s.UserID != 7 || s.Method != "password" || s.IP != "192.0.2.1" || s.UserAgent != "ua" ||
		s.TokenVersion != 3 || !s.CreatedAt.Equal(c.now) || !s.LastSeen.Equal(c.now) {
		t.Fatalf("session %+v", s)
	}
	// Only the hash is stored.
	for _, k := range c.mr.Keys() {
		if strings.Contains(k, tok) || strings.Contains(k, strings.TrimPrefix(tok, SessionPrefix)) {
			t.Fatalf("token in key %q", k)
		}
	}
	if !c.mr.Exists(sessKey(Hash(tok))) || c.mr.TTL(sessKey(Hash(tok))) != 7*24*time.Hour {
		t.Fatalf("session key ttl %v", c.mr.TTL(sessKey(Hash(tok))))
	}
	gone(t, v, SessionPrefix+"unknown")
}

// A session ends unused for the idle timeout; used, it is renewed, but not
// past the absolute timeout.
func TestIdleAndAbsoluteTimeout(t *testing.T) {
	v, c := newStore(t)
	idle := create(t, v, 7, "idle")
	c.advance(7*24*time.Hour + time.Second)
	gone(t, v, idle)

	used := create(t, v, 7, "used")
	touch := func() {
		t.Helper()
		if err := v.Touch(ctx, used, mustLookup(t, v, used)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ { // 4 x 6 days: past the idle timeout
		c.advance(6 * 24 * time.Hour)
		touch()
	}
	c.advance(5 * 24 * time.Hour) // day 29: renewed to the absolute timeout only
	touch()
	if ttl := c.mr.TTL(sessKey(Hash(used))); ttl != 24*time.Hour {
		t.Fatalf("renewed past the absolute timeout: ttl %v", ttl)
	}
	c.advance(24*time.Hour + time.Second)
	gone(t, v, used)
}

// A use is written only TouchEvery after the last written one.
func TestTouchThrottled(t *testing.T) {
	v, c := newStore(t)
	tok := create(t, v, 7, "fam")
	c.advance(4 * time.Minute)
	if err := v.Touch(ctx, tok, mustLookup(t, v, tok)); err != nil {
		t.Fatal(err)
	}
	if s := mustLookup(t, v, tok); !s.LastSeen.Equal(c.now.Add(-4 * time.Minute)) {
		t.Fatalf("written before TouchEvery: last seen %v", s.LastSeen)
	}
	c.advance(2 * time.Minute)
	if err := v.Touch(ctx, tok, mustLookup(t, v, tok)); err != nil {
		t.Fatal(err)
	}
	if s := mustLookup(t, v, tok); !s.LastSeen.Equal(c.now) {
		t.Fatalf("not written after TouchEvery: last seen %v", s.LastSeen)
	}
	if ttl := c.mr.TTL(sessKey(Hash(tok))); ttl != 7*24*time.Hour {
		t.Fatalf("ttl not renewed: %v", ttl)
	}
}

// A written use records where the session was used from, so the sessions
// list shows a copy of a token in use elsewhere; an unwritten one does not.
func TestTouchRecordsWhereUsed(t *testing.T) {
	v, c := newStore(t)
	tok := create(t, v, 7, "fam")
	from := func(ip, ua string) Session {
		s := mustLookup(t, v, tok)
		s.IP, s.UserAgent = ip, ua
		return s
	}
	c.advance(time.Minute)
	require.NoError(t, v.Touch(ctx, tok, from("198.51.100.7", "elsewhere")))
	if s := mustLookup(t, v, tok); s.IP != "192.0.2.1" || s.UserAgent != "ua" {
		t.Fatalf("written before TouchEvery: %+v", s)
	}
	c.advance(5 * time.Minute)
	require.NoError(t, v.Touch(ctx, tok, from("198.51.100.7", "elsewhere")))
	s := mustLookup(t, v, tok)
	assert.Equal(t, "198.51.100.7", s.IP)
	assert.Equal(t, "elsewhere", s.UserAgent)
	assert.True(t, s.LastSeen.Equal(c.now))
	assert.Equal(t, "password", s.Method)
	l, err := v.List(ctx, 7)
	require.NoError(t, err)
	require.Len(t, l, 1)
	assert.Equal(t, "198.51.100.7", l[0].IP)
}

// A rotation starts from the token the caller presented: one another
// rotation already left in its grace (another tab verified first) rotates
// nothing, so the token that rotation issued is neither ended nor cut to
// the grace.
func TestRotateFromSupersededToken(t *testing.T) {
	v, c := newStore(t)
	t0 := create(t, v, 7, "fam")
	t1, err := v.Rotate(ctx, 7, "fam", t0)
	require.NoError(t, err)
	_, err = v.Rotate(ctx, 7, "fam", t0)
	require.ErrorIs(t, err, ErrSuperseded)
	assert.Greater(t, c.mr.TTL(sessKey(Hash(t1))), DefaultRotateGrace)
	mustLookup(t, v, t0) // still in its grace
	t2, err := v.Rotate(ctx, 7, "fam", t1)
	require.NoError(t, err)
	gone(t, v, t0)
	mustLookup(t, v, t2)
}

// Drop checks the stamp again: a session a keeping bump restamped after it
// was read (the version is the bump's) stays, both tokens of a rotation;
// one still stamped below goes.
func TestDropSparesARestampedSession(t *testing.T) {
	v, _ := newStore(t)
	t0 := create(t, v, 7, "keep") // tv 3
	t1, err := v.Rotate(ctx, 7, "keep", t0)
	require.NoError(t, err)
	read0, read1 := mustLookup(t, v, t0), mustLookup(t, v, t1)
	require.NoError(t, v.Revoke(ctx, 7, Selection{Keep: "keep"}, Bump{From: 3, To: 4}))
	for _, x := range []struct {
		tok string
		s   Session
	}{{t0, read0}, {t1, read1}} {
		dropped, err := v.Drop(ctx, x.tok, x.s, 4)
		require.NoError(t, err)
		assert.False(t, dropped)
		mustLookup(t, v, x.tok)
	}
	dropped, err := v.Drop(ctx, t1, read1, 5)
	require.NoError(t, err)
	assert.True(t, dropped)
	gone(t, v, t1)
}

// Prune ends what is stamped below the version whatever the clocks say: a
// session another node (its clock ahead) created is not taken for one
// issued after the call.
func TestPruneIgnoresClocks(t *testing.T) {
	v, c := newStore(t)
	ahead := NewValkey(v.rdb, Options{Now: func() time.Time { return c.now.Add(time.Hour) }})
	stale := create(t, ahead, 7, "stale") // tv 3, created an hour "later"
	kept, err := v.Create(ctx, Session{ID: "kept", UserID: 7, TokenVersion: 4})
	require.NoError(t, err)
	keptNext, err := v.Rotate(ctx, 7, "kept", kept)
	require.NoError(t, err)
	require.NoError(t, v.Prune(ctx, 7, 4))
	gone(t, v, stale)
	mustLookup(t, v, kept)
	mustLookup(t, v, keptNext)
}

// Revoking one session ends it at once; a touch does not bring it back.
func TestRevokeOne(t *testing.T) {
	v, c := newStore(t)
	a, b := create(t, v, 7, "a"), create(t, v, 7, "b")
	sa := mustLookup(t, v, a)
	if err := v.Revoke(ctx, 7, Selection{Only: "a"}, Bump{}); err != nil {
		t.Fatal(err)
	}
	gone(t, v, a)
	mustLookup(t, v, b)
	c.advance(time.Hour)
	if err := v.Touch(ctx, a, sa); err != nil {
		t.Fatal(err)
	}
	gone(t, v, a)
	if c.mr.Exists(sessKey(Hash(a))) {
		t.Fatal("touch recreated a revoked session")
	}
	// Another user's id is not theirs to end.
	if err := v.Revoke(ctx, 8, Selection{Only: "b"}, Bump{}); err != nil {
		t.Fatal(err)
	}
	mustLookup(t, v, b)
}

// Revoking all but one ends the others and stamps the survivor with the new
// token version; a lower version never overwrites a higher one.
func TestRevokeOthersAndAll(t *testing.T) {
	v, _ := newStore(t)
	keep, x, y := create(t, v, 7, "keep"), create(t, v, 7, "x"), create(t, v, 7, "y")
	other := create(t, v, 8, "other")
	if err := v.Revoke(ctx, 7, Selection{Keep: "keep"}, Bump{From: 3, To: 5}); err != nil {
		t.Fatal(err)
	}
	gone(t, v, x)
	gone(t, v, y)
	if s := mustLookup(t, v, keep); s.TokenVersion != 5 {
		t.Fatalf("survivor tv %d", s.TokenVersion)
	}
	if err := v.Revoke(ctx, 7, Selection{Only: "nothing"}, Bump{From: 3, To: 4}); err != nil {
		t.Fatal(err)
	}
	if s := mustLookup(t, v, keep); s.TokenVersion != 5 {
		t.Fatalf("tv went back to %d", s.TokenVersion)
	}
	if err := v.Revoke(ctx, 7, Selection{}, Bump{From: 5, To: 6}); err != nil {
		t.Fatal(err)
	}
	gone(t, v, keep)
	mustLookup(t, v, other)
	if l, err := v.List(ctx, 7); err != nil || len(l) != 0 {
		t.Fatalf("list after revoking all: %v, %v", l, err)
	}
}

// The index forgets sessions that expired on their own: when listing, and
// at the next login.
func TestListDropsExpired(t *testing.T) {
	v, c := newStore(t)
	create(t, v, 7, "old")
	c.advance(3 * 24 * time.Hour)
	recent := create(t, v, 7, "recent")
	c.advance(4*24*time.Hour + time.Second) // "old" idles out
	l, err := v.List(ctx, 7)
	if err != nil || len(l) != 1 || l[0].ID != "recent" {
		t.Fatalf("list %+v, %v", l, err)
	}
	if ids, _ := c.mr.ZMembers(userKey(7)); len(ids) != 1 || ids[0] != "recent" {
		t.Fatalf("index %v", ids)
	}
	// An index entry whose keys are gone without a trace is dropped too.
	if _, err := c.mr.ZAdd(userKey(7), 1, "ghost"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	newer := create(t, v, 7, "newer")
	c.advance(time.Minute)
	if err := v.Touch(ctx, recent, mustLookup(t, v, recent)); err != nil {
		t.Fatal(err)
	}
	l, err = v.List(ctx, 7)
	if err != nil || len(l) != 2 || l[0].ID != "recent" || l[1].ID != "newer" {
		t.Fatalf("list %+v, %v", l, err)
	}
	if ok, _ := c.mr.ZMembers(userKey(7)); len(ok) != 2 {
		t.Fatalf("ghost kept: %v", ok)
	}
	if has, err := v.Has(ctx, 7, "newer", 3); err != nil || !has {
		t.Fatalf("has newer: %v %v", has, err)
	}
	if has, _ := v.Has(ctx, 8, "newer", 3); has {
		t.Fatal("another user has newer")
	}
	if has, _ := v.Has(ctx, 7, "newer", 4); has {
		t.Fatal("has newer, which a bump ended")
	}
	_ = newer
}

// Rotating keeps the session id and its bounds; the old token works for the
// grace only, and a revocation ends both at once.
func TestRotate(t *testing.T) {
	v, c := newStore(t)
	old := create(t, v, 7, "fam")
	c.advance(6 * time.Minute) // a use of old would be written now
	if _, err := v.Rotate(ctx, 8, "fam", old); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated another user's session: %v", err)
	}
	fresh, err := v.Rotate(ctx, 7, "fam", old)
	if err != nil || fresh == old {
		t.Fatalf("rotate: %q, %v", fresh, err)
	}
	if s := mustLookup(t, v, fresh); s.ID != "fam" || s.UserID != 7 || s.TokenVersion != 3 {
		t.Fatalf("rotated session %+v", s)
	}
	// The old token works for the grace, and its use does not renew it.
	if err := v.Touch(ctx, old, mustLookup(t, v, old)); err != nil {
		t.Fatal(err)
	}
	if ttl := c.mr.TTL(sessKey(Hash(old))); ttl != 30*time.Second {
		t.Fatalf("old token ttl %v", ttl)
	}
	c.advance(31 * time.Second)
	gone(t, v, old)
	mustLookup(t, v, fresh)

	again := create(t, v, 7, "fam2")
	next, err := v.Rotate(ctx, 7, "fam2", again)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Revoke(ctx, 7, Selection{Only: "fam2"}, Bump{}); err != nil {
		t.Fatal(err)
	}
	gone(t, v, again)
	gone(t, v, next)
}

// A flow is read any number of times and taken once.
func TestFlowsOnce(t *testing.T) {
	v, c := newStore(t)
	tok, err := v.PutFlow(ctx, "login_2fa", Flow{UserID: 7, Payload: []byte(`{"a":1}`)})
	if err != nil || strings.HasPrefix(tok, SessionPrefix) {
		t.Fatalf("put %q, %v", tok, err)
	}
	if _, err := v.PeekFlow(ctx, "totp_setup", tok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other purpose: %v", err)
	}
	for i := 0; i < 2; i++ {
		f, err := v.PeekFlow(ctx, "login_2fa", tok)
		if err != nil || f.UserID != 7 || string(f.Payload) != `{"a":1}` {
			t.Fatalf("peek %+v, %v", f, err)
		}
	}
	if _, err := v.TakeFlow(ctx, "login_2fa", tok); err != nil {
		t.Fatal(err)
	}
	if _, err := v.TakeFlow(ctx, "login_2fa", tok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("taken twice: %v", err)
	}
	late, _ := v.PutFlow(ctx, "passkey_login", Flow{Payload: []byte(`{}`)})
	c.advance(5*time.Minute + time.Second)
	if _, err := v.TakeFlow(ctx, "passkey_login", late); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired flow: %v", err)
	}
}

// Valkey down: every call fails with ErrUnavailable, a 503, never with
// ErrNotFound (which would sign the user out).
func TestUnavailable(t *testing.T) {
	v, c := newStore(t)
	tok := create(t, v, 7, "fam")
	s := mustLookup(t, v, tok)
	c.advance(time.Hour)
	c.mr.Close()
	check := func(name string, err error) {
		t.Helper()
		if errors.Is(err, ErrNotFound) || core.AsError(err).Status != http.StatusServiceUnavailable || !IsUnavailable(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := v.Create(ctx, Session{ID: "x", UserID: 7})
	check("create", err)
	_, err = v.Lookup(ctx, tok)
	check("lookup", err)
	check("touch", v.Touch(ctx, tok, s))
	_, err = v.Rotate(ctx, 7, "fam", tok)
	check("rotate", err)
	_, err = v.List(ctx, 7)
	check("list", err)
	_, err = v.Has(ctx, 7, "fam", 0)
	check("has", err)
	check("revoke", v.Revoke(ctx, 7, Selection{}, Bump{From: 0, To: 1}))
	check("prune", v.Prune(ctx, 7, 1))
	_, err = v.Drop(ctx, tok, s, 9)
	check("drop", err)
	_, err = v.PutFlow(ctx, "p", Flow{})
	check("put flow", err)
	_, err = v.PeekFlow(ctx, "p", "t")
	check("peek flow", err)
	_, err = v.TakeFlow(ctx, "p", "t")
	check("take flow", err)
}

// Two rotations within the grace: the first token ends at the second, so
// a revocation ends every token the session had at once.
func TestRotateTwiceThenRevoke(t *testing.T) {
	v, _ := newStore(t)
	t0 := create(t, v, 7, "fam")
	t1, err := v.Rotate(ctx, 7, "fam", t0)
	require.NoError(t, err)
	t2, err := v.Rotate(ctx, 7, "fam", t1)
	require.NoError(t, err)
	gone(t, v, t0)
	mustLookup(t, v, t1) // in its grace
	require.NoError(t, v.Revoke(ctx, 7, Selection{Only: "fam"}, Bump{}))
	gone(t, v, t1)
	gone(t, v, t2)
}

// A stamp given to a kept session reaches the token a rotation left in its
// grace: a request still carrying it is not stale.
func TestRevokeStampsTheRotatedToken(t *testing.T) {
	v, _ := newStore(t)
	t0 := create(t, v, 7, "keep")
	t1, err := v.Rotate(ctx, 7, "keep", t0)
	require.NoError(t, err)
	create(t, v, 7, "other")
	require.NoError(t, v.Revoke(ctx, 7, Selection{Keep: "keep"}, Bump{From: 3, To: 9}))
	assert.Equal(t, 9, mustLookup(t, v, t0).TokenVersion)
	assert.Equal(t, 9, mustLookup(t, v, t1).TokenVersion)
}

// A bump settles the sessions it keeps against the version before it: one
// stamped below was ended by an earlier bump that did not reach Valkey and
// ends now, both tokens of a rotation, instead of being stamped back to
// life; one stamped ahead (a rolled-back bump) stays. Prune ends those
// below a version and leaves the later ones.
func TestBumpEndsWhatAnEarlierBumpEnded(t *testing.T) {
	v, _ := newStore(t)
	stale := create(t, v, 7, "stale") // tv 3
	staleNext, err := v.Rotate(ctx, 7, "stale", stale)
	require.NoError(t, err)
	fresh, err := v.Create(ctx, Session{ID: "fresh", UserID: 7, TokenVersion: 5})
	require.NoError(t, err)
	ahead, err := v.Create(ctx, Session{ID: "ahead", UserID: 7, TokenVersion: 6})
	require.NoError(t, err)
	require.NoError(t, v.Revoke(ctx, 7, Selection{Only: "none"}, Bump{From: 5, To: 6}))
	gone(t, v, stale)
	gone(t, v, staleNext)
	assert.Equal(t, 6, mustLookup(t, v, fresh).TokenVersion)
	assert.Equal(t, 6, mustLookup(t, v, ahead).TokenVersion)

	later, err := v.Create(ctx, Session{ID: "later", UserID: 7, TokenVersion: 8})
	require.NoError(t, err)
	require.NoError(t, v.Prune(ctx, 7, 8))
	gone(t, v, fresh)
	gone(t, v, ahead)
	mustLookup(t, v, later)
	l, err := v.List(ctx, 7)
	require.NoError(t, err)
	assert.Len(t, l, 1)
}

// Drop ends a whole session through its current token, and only itself
// through a token a rotation left behind.
func TestDrop(t *testing.T) {
	v, c := newStore(t)
	t0 := create(t, v, 7, "fam") // tv 3
	t1, err := v.Rotate(ctx, 7, "fam", t0)
	require.NoError(t, err)
	dropped, err := v.Drop(ctx, t0, mustLookup(t, v, t0), 4)
	require.NoError(t, err)
	assert.True(t, dropped)
	gone(t, v, t0)
	mustLookup(t, v, t1)
	dropped, err = v.Drop(ctx, t1, mustLookup(t, v, t1), 4)
	require.NoError(t, err)
	assert.True(t, dropped)
	gone(t, v, t1)
	assert.False(t, c.mr.Exists(sidKey("fam")))
	ids, _ := c.mr.ZMembers(userKey(7))
	assert.Empty(t, ids)
}

// A user keeps MaxSessions sessions: a login past them ends the one used
// least recently. Creating a session under an id in use ends the old one.
func TestCreateBoundsTheSessions(t *testing.T) {
	mr := miniredis.RunT(t)
	c := &clock{mr: mr, now: time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	v := NewValkey(rdb, Options{MaxSessions: 3, Now: c.Now})
	a := create(t, v, 7, "a")
	c.advance(time.Minute)
	b := create(t, v, 7, "b")
	c.advance(time.Minute)
	x := create(t, v, 7, "c")
	c.advance(10 * time.Minute)
	require.NoError(t, v.Touch(ctx, a, mustLookup(t, v, a))) // b is now the least recent
	d := create(t, v, 7, "d")
	gone(t, v, b)
	mustLookup(t, v, a)
	mustLookup(t, v, x)
	mustLookup(t, v, d)
	l, err := v.List(ctx, 7)
	require.NoError(t, err)
	assert.Len(t, l, 3)

	again := create(t, v, 7, "a")
	gone(t, v, a)
	assert.Equal(t, "a", mustLookup(t, v, again).ID)
}

// A touch after the session's key went does not write a partial one.
func TestTouchDoesNotRecreate(t *testing.T) {
	v, c := newStore(t)
	tok := create(t, v, 7, "fam")
	s := mustLookup(t, v, tok)
	c.advance(time.Hour)
	c.mr.Del(sessKey(Hash(tok)))
	require.NoError(t, v.Touch(ctx, tok, s))
	assert.False(t, c.mr.Exists(sessKey(Hash(tok))))
}

// Revoking one session with no stamp to give reads that session only: the
// rest of the index is left as it is. With a stamp to give, even from
// version 0, every session is read and the others are stamped.
func TestRevokeOnlyReadsOneSession(t *testing.T) {
	v, c := newStore(t)
	create(t, v, 7, "a")
	_, err := c.mr.ZAdd(userKey(7), 1, "ghost")
	require.NoError(t, err)
	require.NoError(t, v.Revoke(ctx, 7, Selection{Only: "a"}, Bump{}))
	ids, _ := c.mr.ZMembers(userKey(7))
	assert.Equal(t, []string{"ghost"}, ids)

	x, err := v.Create(ctx, Session{ID: "x", UserID: 7})
	require.NoError(t, err)
	y, err := v.Create(ctx, Session{ID: "y", UserID: 7})
	require.NoError(t, err)
	require.NoError(t, v.Revoke(ctx, 7, Selection{Only: "x"}, Bump{From: 0, To: 1}))
	gone(t, v, x)
	assert.Equal(t, 1, mustLookup(t, v, y).TokenVersion)
}

// A revocation Valkey runs late, after the caller gave up on it, leaves
// the sessions issued since alone: one stamped past the bump (the
// caller's stampless fallback committed) and one created since below it
// (the caller's change rolled back), the latter unstamped. One stamped to
// the bump's version is never spared: it may be a session from before the
// call that a node with its clock ahead created.
func TestLateRevokeLeavesLaterSessions(t *testing.T) {
	v, c := newStore(t) // sessions stamped 3
	old, keep := create(t, v, 7, "old"), create(t, v, 7, "keep")
	called := c.now
	c.advance(time.Second)
	afterFallback, err := v.Create(ctx, Session{ID: "after-fallback", UserID: 7, TokenVersion: 5})
	require.NoError(t, err)
	afterRollback := create(t, v, 7, "after-rollback")
	stampedTo, err := v.Create(ctx, Session{ID: "stamped-to", UserID: 7, TokenVersion: 4})
	require.NoError(t, err)

	c.now = called // the call as it was made
	require.NoError(t, v.Revoke(ctx, 7, Selection{Keep: "keep"}, Bump{From: 3, To: 4}))
	gone(t, v, old)
	gone(t, v, stampedTo)
	assert.Equal(t, 4, mustLookup(t, v, keep).TokenVersion)
	assert.Equal(t, 5, mustLookup(t, v, afterFallback).TokenVersion)
	assert.Equal(t, 3, mustLookup(t, v, afterRollback).TokenVersion)
}

// The client NewClient makes answers a 503 within about authTimeout when
// the server accepts connections but never answers, instead of the default
// retries and read timeouts (12 s and more).
func TestNewClientFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	accepted := make(chan []net.Conn, 1)
	go func() {
		var conns []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				accepted <- conns
				return
			}
			conns = append(conns, conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for _, c := range <-accepted {
			_ = c.Close()
		}
	})
	rdb := NewClient(&redis.Options{Addr: ln.Addr().String()})
	t.Cleanup(func() { _ = rdb.Close() })
	v := NewValkey(rdb, Options{})
	start := time.Now()
	_, err = v.Lookup(ctx, "s2s_x")
	assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// lossyProxy relays a client's connections to upstream. Armed, it drops the
// connection instead of relaying the answer of the next script the server
// ran (not a NOSCRIPT error): what a timeout after the server executed a
// call looks like to the client, which then retries it.
type lossyProxy struct {
	ln       net.Listener
	upstream string
	armed    atomic.Bool
}

func newLossyProxy(t *testing.T, upstream string) *lossyProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &lossyProxy{ln: ln, upstream: upstream}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.relay(conn)
		}
	}()
	return p
}

func (p *lossyProxy) relay(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	defer func() { _ = client.Close(); _ = server.Close() }()
	var script atomic.Bool // a script call is waiting for its answer
	go func() {
		defer func() { _ = server.Close() }()
		buf := make([]byte, 64<<10)
		for {
			n, err := client.Read(buf)
			if err != nil {
				return
			}
			script.Store(p.armed.Load() && bytes.Contains(bytes.ToUpper(buf[:n]), []byte("EVAL")))
			if _, err := server.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := server.Read(buf)
		if err != nil {
			return
		}
		if script.Load() && buf[0] != '-' && p.armed.CompareAndSwap(true, false) {
			return
		}
		if _, err := client.Write(buf[:n]); err != nil {
			return
		}
	}
}

// A rotation the client retries after its answer was lost (NewClient
// retries once) is not made twice: the session's previous token stays in
// its grace and the new one lives on.
func TestRotateRetriedIsMadeOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	p := newLossyProxy(t, mr.Addr())
	rdb := NewClient(&redis.Options{Addr: p.ln.Addr().String()})
	t.Cleanup(func() { _ = rdb.Close() })
	v := NewValkey(rdb, Options{})
	t0 := create(t, v, 7, "fam")
	p.armed.Store(true)
	t1, err := v.Rotate(ctx, 7, "fam", t0)
	require.NoError(t, err)
	require.False(t, p.armed.Load(), "no answer was lost")
	mustLookup(t, v, t0)
	assert.Equal(t, "fam", mustLookup(t, v, t1).ID)
	assert.Greater(t, mr.TTL(sessKey(Hash(t1))), DefaultRotateGrace)
}

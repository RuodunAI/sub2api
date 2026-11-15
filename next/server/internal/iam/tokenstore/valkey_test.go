package tokenstore

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

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

// Revoking one session ends it at once; a touch does not bring it back.
func TestRevokeOne(t *testing.T) {
	v, c := newStore(t)
	a, b := create(t, v, 7, "a"), create(t, v, 7, "b")
	sa := mustLookup(t, v, a)
	if err := v.Revoke(ctx, 7, Selection{Only: "a"}, 0); err != nil {
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
	if err := v.Revoke(ctx, 8, Selection{Only: "b"}, 0); err != nil {
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
	if err := v.Revoke(ctx, 7, Selection{Keep: "keep"}, 5); err != nil {
		t.Fatal(err)
	}
	gone(t, v, x)
	gone(t, v, y)
	if s := mustLookup(t, v, keep); s.TokenVersion != 5 {
		t.Fatalf("survivor tv %d", s.TokenVersion)
	}
	if err := v.Revoke(ctx, 7, Selection{Only: "nothing"}, 4); err != nil {
		t.Fatal(err)
	}
	if s := mustLookup(t, v, keep); s.TokenVersion != 5 {
		t.Fatalf("tv went back to %d", s.TokenVersion)
	}
	if err := v.Revoke(ctx, 7, Selection{}, 6); err != nil {
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
	if has, err := v.Has(ctx, 7, "newer"); err != nil || !has {
		t.Fatalf("has newer: %v %v", has, err)
	}
	if has, _ := v.Has(ctx, 8, "newer"); has {
		t.Fatal("another user has newer")
	}
	_ = newer
}

// Rotating keeps the session id and its bounds; the old token works for the
// grace only, and a revocation ends both at once.
func TestRotate(t *testing.T) {
	v, c := newStore(t)
	old := create(t, v, 7, "fam")
	c.advance(6 * time.Minute) // a use of old would be written now
	if _, err := v.Rotate(ctx, 8, "fam"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated another user's session: %v", err)
	}
	fresh, err := v.Rotate(ctx, 7, "fam")
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
	next, err := v.Rotate(ctx, 7, "fam2")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Revoke(ctx, 7, Selection{Only: "fam2"}, 0); err != nil {
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

// A window stays open idle after each use, at most max after it opened.
func TestWindow(t *testing.T) {
	v, c := newStore(t)
	idle, max := time.Hour, 3*time.Hour
	if ok, err := v.ContinueWindow(ctx, "w", idle, max); err != nil || ok {
		t.Fatalf("unopened window: %v %v", ok, err)
	}
	if err := v.OpenWindow(ctx, "w", idle); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		c.advance(50 * time.Minute)
		if ok, err := v.ContinueWindow(ctx, "w", idle, max); err != nil || !ok {
			t.Fatalf("continue %d: %v %v", i, ok, err)
		}
	}
	c.advance(50 * time.Minute) // 2.5h in: capped at 3h
	if ok, _ := v.ContinueWindow(ctx, "w", idle, max); !ok || c.mr.TTL(windowKey("w")) != 30*time.Minute {
		t.Fatalf("not capped: ttl %v", c.mr.TTL(windowKey("w")))
	}
	c.advance(31 * time.Minute)
	if ok, _ := v.ContinueWindow(ctx, "w", idle, max); ok {
		t.Fatal("continued past max")
	}
	// Opening again starts afresh.
	if err := v.OpenWindow(ctx, "w", idle); err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.ContinueWindow(ctx, "w", idle, max); !ok {
		t.Fatal("reopened window")
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
		if errors.Is(err, ErrNotFound) || core.AsError(err).Status != http.StatusServiceUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := v.Create(ctx, Session{ID: "x", UserID: 7})
	check("create", err)
	_, err = v.Lookup(ctx, tok)
	check("lookup", err)
	check("touch", v.Touch(ctx, tok, s))
	_, err = v.Rotate(ctx, 7, "fam")
	check("rotate", err)
	_, err = v.List(ctx, 7)
	check("list", err)
	_, err = v.Has(ctx, 7, "fam")
	check("has", err)
	check("revoke", v.Revoke(ctx, 7, Selection{}, 1))
	_, err = v.PutFlow(ctx, "p", Flow{})
	check("put flow", err)
	_, err = v.PeekFlow(ctx, "p", "t")
	check("peek flow", err)
	_, err = v.TakeFlow(ctx, "p", "t")
	check("take flow", err)
	check("open window", v.OpenWindow(ctx, "w", time.Hour))
	_, err = v.ContinueWindow(ctx, "w", time.Hour, time.Hour)
	check("continue window", err)
}

package casbinx

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// fakeDB is a policy store moving through versions: Policy and UserRoles
// read the state of the version current at the call, as the Loader's
// snapshot transactions read PostgreSQL. With churn, every read is followed
// by a write (the version moves on after each read); with churnLeft, the
// next churnLeft reads are. policyDelay is how long a Policy read takes.
type fakeDB struct {
	mu          sync.Mutex
	v           int64
	states      map[int64]dbState
	churn       bool
	churnLeft   int
	policyDelay time.Duration
}

// dbState is the policy and the users' roles at one version.
type dbState struct {
	rules []Rule
	users map[int64]UserRoles
}

func (d *fakeDB) at(v int64) {
	d.mu.Lock()
	d.v = v
	d.mu.Unlock()
}

// read returns the current version and its state (the newest one at or
// below it), moving on when churning.
func (d *fakeDB) read() (int64, dbState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.v
	if d.churn || d.churnLeft > 0 {
		d.v++
		d.churnLeft--
	}
	for at := v; at > 0; at-- {
		if st, ok := d.states[at]; ok {
			return v, st
		}
	}
	return v, dbState{}
}

// bump is a write: the version moves on, the state stays.
func (d *fakeDB) bump() {
	d.mu.Lock()
	d.v++
	d.mu.Unlock()
}

func (d *fakeDB) Policy(context.Context) (Policy, error) {
	time.Sleep(d.policyDelay)
	v, st := d.read()
	return Policy{Version: v, Active: activeOf(st.rules), Rules: st.rules}, nil
}

func (d *fakeDB) UserRoles(_ context.Context, uid int64) (UserRoles, error) {
	v, st := d.read()
	ur := st.users[uid]
	ur.Version = v
	return ur, nil
}

// testResolver is the resolver over d with the real role cache (no L2).
func testResolver(d *fakeDB, self ...string) (*resolver, *roleCache) {
	rc := newRoleCache(d, nil, roleCacheOptions{})
	return newResolver(d, rc, self, snapshotOptions{Wait: time.Second, Debounce: time.Millisecond}), rc
}

func TestResolverGrants(t *testing.T) {
	t.Parallel()
	d := &fakeDB{v: 4, states: map[int64]dbState{4: {
		rules: []Rule{grant("r:t", "account:read")},
		users: map[int64]UserRoles{1: held("r:t"), 2: {Roles: []string{"r:t"}}},
	}}}
	r, _ := testResolver(d, "self:profile")
	ctx := context.Background()
	set, err := r.Grants(ctx, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.KeyList(); !slices.Equal(got, []string{"account:read", "self:profile"}) || set.Version != 4 {
		t.Fatalf("Grants = %v at %d", got, set.Version)
	}
	if set, err := r.Grants(ctx, 2, 3); err != nil || len(set.KeyList()) != 0 || set.Version != 4 {
		t.Fatalf("inactive user: %+v, %v", set, err)
	}
	if ok, v, err := Enforce(ctx, r, 1, "account:read:own", 3); !ok || v != 4 || err != nil {
		t.Fatalf("Enforce = %v at %d, %v", ok, v, err)
	}
	if ok, _, err := Enforce(ctx, r, 2, "self:profile", 3); ok || err != nil {
		t.Fatalf("Enforce of an inactive user = %v, %v", ok, err)
	}
	// A snapshot below min, never reaching it: fail closed.
	if _, err := r.Grants(ctx, 1, 5); !errors.Is(err, ErrStale) {
		t.Fatalf("snapshot below min: %v", err)
	}
}

// One write at v6 revokes r:a from the user and grants r:a user:read. No
// set may combine the v5 link with the v6 grant (user:read, held at
// neither version), whichever half the caches hold from before the write.
func TestResolverTornRead(t *testing.T) {
	t.Parallel()
	const uid = 7
	d := &fakeDB{v: 5, states: map[int64]dbState{
		5: {rules: []Rule{grant("r:a", "account:read")}, users: map[int64]UserRoles{uid: held("r:a")}},
		6: {rules: []Rule{grant("r:a", "account:read"), grant("r:a", "user:read")}, users: map[int64]UserRoles{uid: held()}},
	}}
	ctx := context.Background()
	check := func(name string, set setOrErr, wantV int64, want []string) {
		t.Helper()
		if set.err != nil {
			t.Fatalf("%s: %v", name, set.err)
		}
		if got := set.s.KeyList(); set.s.Version != wantV || !slices.Equal(got, want) || set.s.Has("user:read") && set.s.Version == 5 {
			t.Fatalf("%s: %v at v%d, want %v at v%d", name, got, set.s.Version, want, wantV)
		}
	}

	// The roles cached at v5, the snapshot built after the write: the
	// roles are read again at the snapshot's version.
	r, rc := testResolver(d)
	if _, err := rc.Warm(ctx, uid, 5); err != nil {
		t.Fatal(err)
	}
	d.at(6)
	check("roles older", grantsOf(ctx, r, uid, 5), 6, nil)

	// The snapshot built at v5, the roles cached after the write (another
	// request warmed them): the snapshot is rebuilt at the roles' version.
	d.at(5)
	r, rc = testResolver(d)
	check("before the write", grantsOf(ctx, r, uid, 5), 5, []string{"account:read"})
	d.at(6)
	if _, err := rc.Warm(ctx, uid, 6); err != nil {
		t.Fatal(err)
	}
	check("snapshot older", grantsOf(ctx, r, uid, 5), 6, nil)
	if ok, v, err := Enforce(ctx, r, uid, "user:read", 5); ok || v != 6 || err != nil {
		t.Fatalf("Enforce(user:read) = %v at v%d, %v", ok, v, err)
	}
}

type setOrErr struct {
	s   core.PermissionSet
	err error
}

func grantsOf(ctx context.Context, r Resolver, uid, min int64) setOrErr {
	s, err := r.Grants(ctx, uid, min)
	return setOrErr{s, err}
}

// When writes keep landing between the two reads until the snapshot wait
// runs out, the resolution fails with ErrSkew rather than combine them.
func TestResolverSkewFailsClosed(t *testing.T) {
	t.Parallel()
	d := &fakeDB{v: 1, churn: true, states: map[int64]dbState{
		1: {rules: []Rule{grant("r:t", "account:read")}, users: map[int64]UserRoles{1: held("r:t")}},
	}}
	r, _ := testResolver(d)
	ctx := context.Background()
	if _, err := r.Grants(ctx, 1, 1); !errors.Is(err, ErrSkew) {
		t.Fatalf("Grants = %v, want ErrSkew", err)
	}
	if _, _, err := Enforce(ctx, r, 1, "account:read", 1); !errors.Is(err, ErrSkew) {
		t.Fatalf("Enforce = %v, want ErrSkew", err)
	}
	// Quiet again: both reads meet.
	d.mu.Lock()
	d.churn = false
	d.mu.Unlock()
	if set, err := r.Grants(ctx, 1, 1); err != nil || !set.Has("account:read") {
		t.Fatalf("Grants = %+v, %v", set, err)
	}
}

// Writes landing between the two reads for round after round (a burst):
// the resolution reads again, pausing longer each time, until both halves
// meet, instead of failing the request after two rounds.
func TestResolverRidesOutSkew(t *testing.T) {
	t.Parallel()
	d := &fakeDB{v: 1, churnLeft: 12, states: map[int64]dbState{
		1: {rules: []Rule{grant("r:t", "account:read")}, users: map[int64]UserRoles{1: held("r:t")}},
	}}
	r, _ := testResolver(d)
	ctx := context.Background()
	set, err := r.Grants(ctx, 1, 1)
	if err != nil || !set.Has("account:read") {
		t.Fatalf("Grants = %+v, %v", set, err)
	}
	d.mu.Lock()
	left := d.churnLeft
	d.mu.Unlock()
	if left > 6 {
		t.Fatalf("resolved after %d skewed reads only", 12-left)
	}
	d.mu.Lock()
	d.churnLeft = 12
	d.mu.Unlock()
	if ok, _, err := Enforce(ctx, r, 1, "account:read", set.Version); !ok || err != nil {
		t.Fatalf("Enforce = %v, %v", ok, err)
	}
	// Writes without end: a caller's end still ends the wait.
	d = &fakeDB{v: 1, churn: true, states: d.states}
	r, _ = testResolver(d)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := r.Grants(cctx, 1, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Grants past the caller's deadline = %v", err)
	}
}

// Sustained writes, as a bulk import makes (a version bump every 2 ms,
// reads of the policy that take 1 ms, so that a round often straddles a
// write): every resolution made meanwhile succeeds, none fails closed.
func TestResolverUnderSustainedWrites(t *testing.T) {
	t.Parallel()
	d := &fakeDB{v: 1, policyDelay: time.Millisecond, states: map[int64]dbState{
		1: {rules: []Rule{grant("r:t", "account:read")}, users: map[int64]UserRoles{1: held("r:t"), 2: held("r:t")}},
	}}
	r, _ := testResolver(d)
	ctx := context.Background()
	stop := make(chan struct{})
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				d.bump()
			}
		}
	}()
	deadline := time.Now().Add(300 * time.Millisecond)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var calls int
	var failures []error
	for uid := range int64(4) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				d.mu.Lock()
				min := d.v
				d.mu.Unlock()
				_, err := r.Grants(ctx, uid%2+1, min)
				mu.Lock()
				calls++
				if err != nil {
					failures = append(failures, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-writer
	if len(failures) > 0 {
		t.Fatalf("%d of %d resolutions failed under writes, the first: %v", len(failures), calls, failures[0])
	}
}

// foreign is a Resolver of another package's making (a test double).
type foreign struct{}

func (foreign) Grants(context.Context, int64, int64) (core.PermissionSet, error) {
	return core.PermissionSetOf("account:read"), nil
}
func (foreign) Prewarm(int64)                       {}
func (foreign) Reread(context.Context, int64) error { return nil }

// Enforce refuses a Resolver it did not make, with an error rather than a
// panic: only its own has a matcher path, and a double's Grants is not one.
func TestEnforceForeignResolver(t *testing.T) {
	t.Parallel()
	if ok, _, err := Enforce(context.Background(), foreign{}, 1, "account:read", 0); ok || !errors.Is(err, ErrForeignResolver) {
		t.Fatalf("Enforce on a foreign Resolver = %v, %v", ok, err)
	}
}

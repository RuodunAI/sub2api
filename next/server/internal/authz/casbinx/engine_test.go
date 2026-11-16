package casbinx

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLoader is a database: the policy (p rows), and the users' g rows
// and status (users), each user read at the policy's versions or its own
// later one. It counts the reads.
type fakeLoader struct {
	mu        sync.Mutex
	policy    Policy
	users     map[int64]UserRoles
	err       error
	block     chan struct{} // when set, Policy waits for it
	policies  atomic.Int64
	userReads atomic.Int64
}

func (f *fakeLoader) Policy(ctx context.Context) (Policy, error) {
	f.policies.Add(1)
	f.mu.Lock()
	block, err, p := f.block, f.err, f.policy
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return Policy{}, ctx.Err()
		}
	}
	return p, err
}

func (f *fakeLoader) UserRoles(_ context.Context, uid int64) (UserRoles, error) {
	f.userReads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	ur := f.users[uid]
	ur.Version, ur.PolicyVersion = max(ur.Version, f.policy.Version), f.policy.PolicyVersion
	if !ur.Active {
		ur.Roles = nil
	}
	return ur, f.err
}

// set changes the database under f.
func (f *fakeLoader) set(fn func(p *Policy, users map[int64]UserRoles)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.policy, f.users)
}

// twoUsers is a database where user 1 holds r:a (account:read) and user 2
// r:b (proxy:read), at version 1, policy 1.
func twoUsers() *fakeLoader {
	return &fakeLoader{policy: Policy{Version: 1, PolicyVersion: 1,
		Active: map[string]bool{"account:read": true, "proxy:read": true, "settings:manage": true},
		Rules:  []Rule{grant("r:a", "account:read"), grant("r:b", "proxy:read")},
		Roles:  map[string]int64{"a": 1, "b": 2}},
		users: map[int64]UserRoles{1: active("r:a"), 2: active("r:b")}}
}

// active is the g rows of an active user holding roles.
func active(roles ...string) UserRoles { return UserRoles{Active: true, Roles: roles} }

func must(t *testing.T, e *engine, uid, min int64, key string, want bool) {
	t.Helper()
	got, err := e.Enforce(context.Background(), uid, min, key)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Enforce(%d, %q) at %d = %v, want %v", uid, key, min, got, want)
	}
}

// reads fails unless l was read policies times for the policy and users
// times for a user.
func reads(t *testing.T, l *fakeLoader, policies, users int64) {
	t.Helper()
	if l.policies.Load() != policies || l.userReads.Load() != users {
		t.Fatalf("policy reads %d, user reads %d; want %d, %d", l.policies.Load(), l.userReads.Load(), policies, users)
	}
}

// eventually waits for cond.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// The first call loads the policy, its p rows alone; each user is read
// when first asked about, once.
func TestEngineLoadsOnFirstCall(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	must(t, e, 2, 1, "account:read", false)
	must(t, e, 2, 1, "proxy:read", true)
	must(t, e, 1, 1, "proxy:read", false)
	reads(t, l, 1, 2)
}

// A change of one user's roles, next to the enforcer's version, reads that
// user alone into the enforcer and drops its decisions only: the policy is
// not loaded again, and another user's decisions stay cached.
func TestEngineUserChangeKeepsPolicy(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	must(t, e, 2, 1, "proxy:read", true)
	l.set(func(p *Policy, users map[int64]UserRoles) {
		p.Version = 2
		users[2] = active("r:b", "r:a")
	})
	e.Changed(Change{Version: 2, Users: []int64{2}})
	must(t, e, 2, 2, "account:read", true)
	st := e.cur.Load()
	reads(t, l, 1, 3)
	if st.version.Load() != 2 {
		t.Fatalf("version %d", st.version.Load())
	}
	if _, err := st.dc.Get("u:1$$account:read$$"); err != nil {
		t.Fatal("user 1's decision dropped with user 2's change")
	}
}

// A user's change takes away the links the user no longer has: a role swapped
// out, then every link (self included) when the user is disabled; the other
// user keeps theirs.
func TestEngineUserChangeRevokes(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	must(t, e, 2, 1, "proxy:read", true)
	l.set(func(p *Policy, users map[int64]UserRoles) {
		p.Version = 2
		users[1] = active("r:b")
	})
	e.Changed(Change{Version: 2, Users: []int64{1}})
	must(t, e, 1, 2, "account:read", false)
	must(t, e, 1, 2, "proxy:read", true)
	l.set(func(p *Policy, users map[int64]UserRoles) {
		p.Version = 3
		users[1] = UserRoles{Roles: []string{"r:b"}}
	})
	e.Changed(Change{Version: 3, Users: []int64{1}})
	must(t, e, 1, 3, "proxy:read", false)
	if roles, _ := e.cur.Load().e.GetRolesForUser("u:1"); len(roles) != 0 {
		t.Fatalf("disabled user still linked to %v", roles)
	}
	must(t, e, 2, 3, "proxy:read", true)
	if l.policies.Load() != 1 {
		t.Fatalf("policy read %d times", l.policies.Load())
	}
}

// A user change whose read meets a newer policy (a policy write committed
// meanwhile) loads the policy instead of joining the user's new roles to
// the old grants.
func TestEngineUserReadUnderNewerPolicyLoads(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 2, 1, "proxy:read", true)
	l.set(func(p *Policy, _ map[int64]UserRoles) {
		p.Version, p.PolicyVersion = 3, 2
		p.Rules = []Rule{grant("r:a", "account:read")}
	})
	e.Changed(Change{Version: 2, Users: []int64{2}})
	must(t, e, 2, 2, "proxy:read", false)
	if l.policies.Load() != 2 || e.cur.Load().policy != 2 {
		t.Fatalf("policy reads %d, policy %d", l.policies.Load(), e.cur.Load().policy)
	}
}

// An inactive user is read too, and holds nothing, the self permissions
// included.
func TestEngineLearnsInactiveUser(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	l.users[3], l.users[4] = active("r:b"), UserRoles{Roles: []string{"r:a"}}
	e := newEngine(l, []string{"self:profile"}, Options{})
	must(t, e, 3, 1, "proxy:read", true)
	must(t, e, 3, 1, "self:profile", true)
	must(t, e, 4, 1, "self:profile", false)
	must(t, e, 4, 1, "account:read", false)
	reads(t, l, 1, 2)
}

// A policy change loads the p rows again into the state, whose users stay
// (no user read again) while their decisions drop; a version skipped, or
// one of unknown extent, may have changed anyone's roles: the state is
// replaced, and its users read again as they are asked about.
func TestEngineLoadsPolicyAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		change Change
		kept   bool
	}{
		"policy":  {Change{Version: 2, Policy: 2}, true},
		"skipped": {Change{Version: 3, Policy: 2}, false},
		"unknown": {Change{Version: 2}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := twoUsers()
			e := newEngine(l, nil, Options{})
			must(t, e, 1, 1, "account:read", true)
			must(t, e, 1, 1, "settings:manage", false)
			before := e.cur.Load()
			l.set(func(p *Policy, _ map[int64]UserRoles) {
				p.Version, p.PolicyVersion = c.change.Version, 2
				p.Rules = append(p.Rules, grant("r:a", "settings:manage"))
			})
			e.Changed(c.change)
			must(t, e, 1, c.change.Version, "settings:manage", true)
			userReads := int64(1)
			if !c.kept {
				userReads = 2
			}
			reads(t, l, 2, userReads)
			if (e.cur.Load() == before) != c.kept {
				t.Fatalf("state kept: %v", !c.kept)
			}
		})
	}
}

// A load that meets, among the versions it covers, a write naming users
// drops those users alone: they are read again, the others are not.
func TestEngineReloadDropsNamedUsers(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	must(t, e, 2, 1, "proxy:read", true)
	l.set(func(p *Policy, users map[int64]UserRoles) {
		p.Version, p.PolicyVersion = 3, 2
		p.Rules = append(p.Rules, grant("r:b", "settings:manage"))
		users[2] = active("r:a")
	})
	e.mu.Lock() // both heard before apply looks
	e.queue = append(e.queue, Change{Version: 3, Users: []int64{2}}, Change{Version: 2, Policy: 2})
	e.known = 3
	e.kick()
	e.mu.Unlock()
	must(t, e, 2, 3, "proxy:read", false)
	must(t, e, 2, 3, "account:read", true)
	must(t, e, 1, 3, "account:read", true)
	reads(t, l, 2, 3)
}

// A role deleted (or deleted and created again under its key) takes its
// members' links with it: the state is replaced, not reloaded.
func TestEngineRoleDeletedReplacesState(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	before := e.cur.Load()
	l.set(func(p *Policy, users map[int64]UserRoles) {
		p.Version, p.PolicyVersion = 2, 2
		p.Roles = map[string]int64{"a": 3, "b": 2}
		users[1] = active()
	})
	e.Changed(Change{Version: 2, Policy: 2})
	must(t, e, 1, 2, "account:read", false)
	if e.cur.Load() == before {
		t.Fatal("the state was reloaded with a deleted role's links")
	}
}

// Past Options.Users, a user read in makes another forgotten; one
// forgotten is read again when next asked about, and decided right.
func TestEngineUsersBounded(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	l.users[3] = active("r:a", "r:b")
	e := newEngine(l, nil, Options{Users: 2})
	for _, uid := range []int64{1, 2, 3, 1, 2, 3} {
		must(t, e, uid, 1, "account:read", uid != 2)
		must(t, e, uid, 1, "proxy:read", uid != 1)
		st := e.cur.Load()
		st.mu.RLock()
		n := len(st.users)
		st.mu.RUnlock()
		if n > 2 {
			t.Fatalf("%d users held", n)
		}
	}
	if l.userReads.Load() <= 3 {
		t.Fatal("no user was forgotten")
	}
}

// A read older than what the state took in since it was made (a reload,
// a user forgotten) is refused: it could bring back a role taken away.
func TestStateRefusesOlderReads(t *testing.T) {
	t.Parallel()
	st := snap(t, grant("r:a", "account:read"))
	if ok, err := st.setUser(1, UserRoles{Version: 0, PolicyVersion: 0, Active: true}); ok || err != nil {
		t.Fatalf("a read older than the load: %v %v", ok, err)
	}
	if ok, err := st.setUser(1, UserRoles{Version: 1, PolicyVersion: 2, Active: true}); ok || err != nil {
		t.Fatalf("a read under another policy: %v %v", ok, err)
	}
	st.as(t, 1, held("r:a"))
	st.mu.Lock()
	err := st.forget(1)
	st.mu.Unlock()
	if err != nil || st.floor != math.MaxInt64 {
		t.Fatalf("floor %d after forgetting a read at the newest version: %v", st.floor, err)
	}
}

// Has of a set Permissions returned never reads the database: once the
// state no longer knows the user (replaced meanwhile), it answers from
// the set's lists, even while the database fails.
func TestPermissionsHasReadsNothing(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	set, err := e.Permissions(context.Background(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	l.set(func(p *Policy, _ map[int64]UserRoles) { p.Version = 2 })
	e.Changed(Change{Version: 2})
	eventually(t, func() bool { return e.cur.Load().version.Load() == 2 })
	l.mu.Lock()
	l.err = errors.New("down")
	l.mu.Unlock()
	n := l.userReads.Load()
	if !set.Has("account:read") || set.Has("proxy:read") || l.userReads.Load() != n {
		t.Fatalf("Has read the database, or answered wrong")
	}
}

// Past the wait, a call fails closed rather than decide from an older
// version; the load that was slow serves the calls after it.
func TestEngineStaleFailsClosed(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{Wait: 50 * time.Millisecond})
	must(t, e, 1, 1, "account:read", true)
	release := make(chan struct{})
	l.set(func(p *Policy, _ map[int64]UserRoles) { p.Version = 2 })
	l.mu.Lock()
	l.block = release
	l.mu.Unlock()
	e.Changed(Change{Version: 2})
	if _, err := e.Enforce(context.Background(), 1, 2, "account:read"); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	must(t, e, 1, 1, "account:read", true) // an older version is still served
	close(release)
	eventually(t, func() bool { return e.cur.Load().version.Load() == 2 })
	must(t, e, 1, 2, "account:read", true)
}

// A failing load is tried again; calls fail closed with its error until it
// works.
func TestEngineLoadErrorRetried(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	boom := errors.New("boom")
	l.err = boom
	e := newEngine(l, nil, Options{Wait: 30 * time.Millisecond})
	if _, err := e.Enforce(context.Background(), 1, 1, "account:read"); !errors.Is(err, ErrStale) || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	l.mu.Lock()
	l.err = nil
	l.mu.Unlock()
	eventually(t, func() bool { return e.cur.Load() != nil })
	must(t, e, 1, 1, "account:read", true)
}

// Decisions are kept per subject, dropped per subject, and bounded.
func TestDecisionCache(t *testing.T) {
	t.Parallel()
	d := &decisions{}
	_ = d.Set("u:1$$a$$", true)
	_ = d.Set("u:2$$a$$", false)
	if v, err := d.Get("u:2$$a$$"); err != nil || v {
		t.Fatalf("u:2 = %v %v", v, err)
	}
	d.dropSubject("u:1")
	if _, err := d.Get("u:1$$a$$"); err == nil {
		t.Fatal("u:1 kept")
	}
	if _, err := d.Get("u:2$$a$$"); err != nil {
		t.Fatal("u:2 dropped with u:1")
	}
	for i := range decisionsPerSubject + 10 {
		_ = d.Set("u:3$$k"+string(rune(i))+"$$", true)
	}
	if v, _ := d.subs.Load("u:3"); v.(*subjectDecisions).n.Load() > decisionsPerSubject {
		t.Fatal("past the subject bound")
	}
	if err := d.Delete("u:2$$a$$"); err != nil || d.Clear() != nil || d.total.Load() != 0 {
		t.Fatalf("total %d after Clear", d.total.Load())
	}
}

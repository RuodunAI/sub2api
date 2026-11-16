package casbinx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
)

// fakeLoader is a database: policy, and users read one at a time (those
// absent are read off the policy's g rows, active when listed in Users). It
// counts the reads.
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
	if ur, ok := f.users[uid]; ok {
		ur.Version, ur.PolicyVersion = max(ur.Version, f.policy.Version), f.policy.PolicyVersion
		return ur, f.err
	}
	ur := UserRoles{Version: f.policy.Version, PolicyVersion: f.policy.PolicyVersion}
	for _, id := range f.policy.Users {
		ur.Active = ur.Active || id == uid
	}
	for _, r := range f.policy.Rules {
		if r.Ptype == "g" && r.V0 == casbinrule.UserSub(uid) && ur.Active {
			ur.Roles = append(ur.Roles, r.V1)
		}
	}
	return ur, f.err
}

// set changes the database under f.
func (f *fakeLoader) set(fn func(p *Policy)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.policy)
}

// twoUsers is a database where user 1 holds r:a (account:read) and user 2
// r:b (proxy:read), at version 1, policy 1.
func twoUsers() *fakeLoader {
	return &fakeLoader{policy: Policy{Version: 1, PolicyVersion: 1,
		Active: map[string]bool{"account:read": true, "proxy:read": true, "settings:manage": true},
		Rules: []Rule{grant("r:a", "account:read"), grant("r:b", "proxy:read"),
			{"g", "u:1", "r:a", ""}, {"g", "u:2", "r:b", ""}},
		Users: []int64{1, 2}}}
}

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

// The first call loads the policy, every active user's g rows with it:
// deciding for them reads no user.
func TestEngineLoadsOnFirstCall(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	e := newEngine(l, nil, Options{})
	must(t, e, 1, 1, "account:read", true)
	must(t, e, 2, 1, "account:read", false)
	must(t, e, 2, 1, "proxy:read", true)
	if l.policies.Load() != 1 || l.userReads.Load() != 0 {
		t.Fatalf("policy reads %d, user reads %d", l.policies.Load(), l.userReads.Load())
	}
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
	l.set(func(p *Policy) {
		p.Version = 2
		p.Rules = append(p.Rules, Rule{"g", "u:2", "r:a", ""})
	})
	e.Changed(Change{Version: 2, Users: []int64{2}})
	must(t, e, 2, 2, "account:read", true)
	st := e.cur.Load()
	if l.policies.Load() != 1 || l.userReads.Load() != 1 || st.version.Load() != 2 {
		t.Fatalf("policy reads %d, user reads %d, version %d", l.policies.Load(), l.userReads.Load(), st.version.Load())
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
	l.set(func(p *Policy) {
		p.Version = 2
		p.Rules[2] = Rule{"g", "u:1", "r:b", ""}
	})
	e.Changed(Change{Version: 2, Users: []int64{1}})
	must(t, e, 1, 2, "account:read", false)
	must(t, e, 1, 2, "proxy:read", true)
	l.set(func(p *Policy) {
		p.Version = 3
		p.Users = []int64{2}
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

// A policy change, a version skipped and a change of unknown extent all
// load the policy again.
func TestEngineLoadsPolicyAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]Change{
		"policy":  {Version: 2, Policy: 2},
		"skipped": {Version: 3, Users: []int64{2}},
		"unknown": {Version: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := twoUsers()
			e := newEngine(l, nil, Options{})
			must(t, e, 1, 1, "account:read", true)
			l.set(func(p *Policy) {
				p.Version, p.PolicyVersion = c.Version, 2
				p.Rules = append(p.Rules, grant("r:a", "settings:manage"))
			})
			e.Changed(c)
			must(t, e, 1, c.Version, "settings:manage", true)
			if l.policies.Load() != 2 {
				t.Fatalf("policy reads %d", l.policies.Load())
			}
		})
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
	l.set(func(p *Policy) {
		p.Version, p.PolicyVersion = 3, 2
		p.Rules = []Rule{grant("r:a", "account:read"), {"g", "u:1", "r:a", ""}, {"g", "u:2", "r:b", ""}}
	})
	e.Changed(Change{Version: 2, Users: []int64{2}})
	must(t, e, 2, 2, "proxy:read", false)
	if l.policies.Load() != 2 || e.cur.Load().policy != 2 {
		t.Fatalf("policy reads %d, policy %d", l.policies.Load(), e.cur.Load().policy)
	}
}

// A user the load did not hold (inactive then, or written without a
// version) is read when first asked about, then known.
func TestEngineLearnsUnknownUser(t *testing.T) {
	t.Parallel()
	l := twoUsers()
	l.users = map[int64]UserRoles{3: {Active: true, Roles: []string{"r:b"}}, 4: {}}
	e := newEngine(l, []string{"self:profile"}, Options{})
	must(t, e, 3, 1, "proxy:read", true)
	must(t, e, 3, 1, "self:profile", true)
	must(t, e, 4, 1, "self:profile", false)
	must(t, e, 4, 1, "proxy:read", false)
	if n := l.userReads.Load(); n != 2 {
		t.Fatalf("user reads %d", n)
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
	l.set(func(p *Policy) { p.Version = 2 })
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

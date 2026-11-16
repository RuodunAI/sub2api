package casbinx

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/casbin/casbin/v3"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// Options tunes the enforcer (tests shorten them). Zero fields take the
// defaults.
type Options struct {
	// Wait bounds how long a call waits for the enforcer to reach the
	// version it needs (default 2s); past it the call fails (closed) rather
	// than decide from an older policy.
	Wait time.Duration
	// LoadTimeout bounds one read of the policy or of a user (default 10s).
	// A load serves every waiter, so it runs on its own context rather
	// than a caller's.
	LoadTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Wait <= 0 {
		o.Wait = 2 * time.Second
	}
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = 10 * time.Second
	}
	return o
}

// listsCapacity bounds the users whose grants a state keeps listed; past
// it a user's list is made on each call.
const listsCapacity = 100_000

// engine is the Enforcer: the current state, and the changes the node
// heard of that it has not applied yet, applied by one goroutine at a time
// (apply) in version order.
type engine struct {
	l    Loader
	self []string
	o    Options
	cur  atomic.Pointer[state]

	mu      sync.Mutex
	known   int64         // the newest version heard of
	queue   []Change      // changes heard of, not applied yet
	running bool          // apply runs
	moved   chan struct{} // closed, and replaced, after each step of apply
	lastErr error         // the error of apply's last step, nil when it worked
}

// New returns the node's Enforcer over l; self lists the self permission
// keys (authz.SelfPermissionKeys), SelfRole's grants. It loads the policy
// on its first call or change.
func New(l Loader, self []string, o Options) Enforcer {
	return newEngine(l, self, o)
}

func newEngine(l Loader, self []string, o Options) *engine {
	return &engine{l: l, self: self, o: o.withDefaults(), moved: make(chan struct{})}
}

// state is one enforcer and what it holds: the policy of one policy
// version, and the g rows of the users it knows, each read at some authz
// version; version is the authz version every change up to which it holds.
// A policy change replaces the state whole; a user change edits it
// (setUser).
type state struct {
	policy  int64
	version atomic.Int64
	e       *casbin.SyncedCachedEnforcer
	dc      *decisions
	// lists caches each user's grants (listOf), as dc caches decisions,
	// up to listsCapacity users.
	lists  sync.Map // uid -> core.PermissionSet
	nLists atomic.Int64

	// mu orders the changes of a user's g rows against what is read from
	// them: decisions and lists are made, and kept, under mu shared, and
	// setUser holds it alone, so no decision or list made before a change
	// is kept after it.
	mu sync.RWMutex
	// users holds the authz version each known user's g rows were read
	// at. The policy load reads every active user; another (inactive, or
	// written without a version, as test fixtures are) is read when first
	// asked about (learn).
	users map[int64]int64
}

// ------------------------------------------------------------ Enforcer

func (e *engine) Enforce(ctx context.Context, uid, min int64, key string) (bool, error) {
	st, err := e.await(ctx, min)
	if err != nil {
		return false, err
	}
	var ok bool
	err = e.withUser(ctx, st, uid, func(st *state, sub string) (err error) {
		ok, err = st.e.Enforce(sub, key)
		return err
	})
	return ok, err
}

func (e *engine) Permissions(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	st, err := e.await(ctx, min)
	if err != nil {
		return core.PermissionSet{}, err
	}
	var set core.PermissionSet
	err = e.withUser(ctx, st, uid, func(st *state, sub string) error {
		if v, ok := st.lists.Load(uid); ok {
			set = v.(core.PermissionSet)
			return nil
		}
		rows, err := st.e.GetImplicitPermissionsForUser(sub)
		if err != nil {
			return err
		}
		set = listOf(rows)
		if st.nLists.Load() < listsCapacity {
			if _, loaded := st.lists.LoadOrStore(uid, set); !loaded {
				st.nLists.Add(1)
			}
		}
		return nil
	})
	if err != nil {
		return core.PermissionSet{}, err
	}
	set.Version = st.version.Load()
	return set.Decided(func(k string) bool { return e.decide(uid, k) }), nil
}

// decide is the Has of a set Permissions returned: Enforce on the current
// state, never older than the set's (states only move forward), failing
// closed.
func (e *engine) decide(uid int64, key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), e.o.Wait)
	defer cancel()
	ok, err := e.Enforce(ctx, uid, 0, key)
	return ok && err == nil
}

func (e *engine) Changed(c Change) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.cur.Load(); st != nil && c.Version <= st.version.Load() {
		return
	}
	e.known = max(e.known, c.Version)
	e.queue = append(e.queue, c)
	e.kick()
}

// ------------------------------------------------------------ waiting

// await returns the state once it holds version min, waiting for apply at
// most o.Wait.
func (e *engine) await(ctx context.Context, min int64) (*state, error) {
	if st := e.cur.Load(); st != nil && st.version.Load() >= min {
		return st, nil
	}
	wctx, cancel := context.WithTimeout(ctx, e.o.Wait)
	defer cancel()
	for {
		e.mu.Lock()
		st := e.cur.Load()
		if st != nil && st.version.Load() >= min {
			e.mu.Unlock()
			return st, nil
		}
		if st == nil || min > e.known {
			// Nothing heard of min (or nothing loaded yet): load the
			// policy, which is at min or newer.
			e.known = max(e.known, min)
			e.queue = append(e.queue, Change{Version: min})
		}
		e.kick()
		moved, lastErr := e.moved, e.lastErr
		e.mu.Unlock()
		select {
		case <-moved:
		case <-wctx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if lastErr != nil {
				return nil, fmt.Errorf("%w: version %d: %w", ErrStale, min, lastErr)
			}
			return nil, fmt.Errorf("%w: version %d", ErrStale, min)
		}
	}
}

// withUser runs fn on a state that knows uid, under its lock shared: st,
// once uid is read into it (learn), or a newer state when uid's read met a
// newer policy. Each round reads uid once; three rounds that keep meeting
// a newer policy give up (fail closed).
func (e *engine) withUser(ctx context.Context, st *state, uid int64, fn func(st *state, sub string) error) error {
	sub := casbinrule.UserSub(uid)
	for range 3 {
		st.mu.RLock()
		if _, ok := st.users[uid]; ok {
			defer st.mu.RUnlock()
			return fn(st, sub)
		}
		st.mu.RUnlock()
		next, err := e.learn(ctx, st, uid)
		if err != nil {
			return err
		}
		st = next
	}
	return fmt.Errorf("%w: user %d: the policy kept moving while it was read", ErrStale, uid)
}

// learn reads uid into st, or, when the read met a newer policy than st's,
// has that policy loaded and returns the state that holds it.
func (e *engine) learn(ctx context.Context, st *state, uid int64) (*state, error) {
	ctx, cancel := context.WithTimeout(ctx, e.o.LoadTimeout)
	defer cancel()
	ur, err := e.l.UserRoles(ctx, uid)
	if err != nil {
		return nil, err
	}
	if ur.PolicyVersion == st.policy {
		return st, st.setUser(uid, ur)
	}
	// Never the roles of one policy with the grants of another (a role
	// deleted and created again under its key would bring its old grants
	// back): the policy is loaded again, and uid read anew under it.
	e.Changed(Change{Version: ur.Version, Policy: ur.PolicyVersion})
	return e.await(ctx, ur.Version)
}

// ------------------------------------------------------------ applying

// kick starts apply unless it runs. Called with e.mu held.
func (e *engine) kick() {
	if !e.running {
		e.running = true
		go e.apply()
	}
}

// step is what apply does next: load the policy, or (users set) apply one
// change of users' roles or status.
type step struct {
	load    bool
	version int64
	users   []int64
}

// apply runs the queued changes in version order, one step at a time,
// until none is left; a failing step is tried again after a growing pause.
func (e *engine) apply() {
	pause := time.Duration(0)
	for {
		e.mu.Lock()
		s, ok := e.next()
		if !ok {
			e.running = false
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
		err := e.run(s)
		e.mu.Lock()
		e.lastErr = err
		close(e.moved)
		e.moved = make(chan struct{})
		e.mu.Unlock()
		if err == nil {
			pause = 0
			continue
		}
		slog.Warn("casbinx: applying an authz change failed", "version", s.version, "err", err)
		pause = min(max(2*pause, 50*time.Millisecond), time.Second)
		time.Sleep(pause)
	}
}

// next takes the next step off the queue: the change next to the state's
// version when it names the users it changed alone (the oldest first,
// such a change first among equals), else a load of the policy (none
// loaded, a policy change, a version skipped or one whose extent is
// unknown), which takes every change it covers. Called with e.mu held.
func (e *engine) next() (step, bool) {
	st := e.cur.Load()
	if st == nil {
		return step{load: true}, true
	}
	v := st.version.Load()
	e.queue = slices.DeleteFunc(e.queue, func(c Change) bool { return c.Version <= v })
	if len(e.queue) == 0 {
		return step{}, false
	}
	slices.SortStableFunc(e.queue, func(a, b Change) int {
		if c := cmp.Compare(a.Version, b.Version); c != 0 {
			return c
		}
		return cmp.Compare(boolRank(!a.UsersOnly()), boolRank(!b.UsersOnly()))
	})
	if c := e.queue[0]; c.Version == v+1 && c.UsersOnly() {
		e.queue = e.queue[1:]
		return step{version: c.Version, users: c.Users}, true
	}
	return step{load: true, version: e.known}, true
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// run does s: a change of users is applied to the state when each user's
// read meets its policy; otherwise (or for a load) the policy is loaded.
func (e *engine) run(s step) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.o.LoadTimeout)
	defer cancel()
	if !s.load {
		st := e.cur.Load()
		done, err := e.setUsers(ctx, st, s.users)
		if err != nil || done {
			if err == nil {
				st.version.Store(max(st.version.Load(), s.version))
			}
			return err
		}
	}
	p, err := e.l.Policy(ctx)
	if err != nil {
		return err
	}
	st, err := build(p, e.self)
	if err != nil {
		return err
	}
	e.cur.Store(st)
	if p.Version < s.version {
		// The database is behind a version heard of: restored from a
		// backup, the versions start over (CONTRACTS §99.4). Calls needing
		// the newer version fail closed until it is reached again.
		return fmt.Errorf("casbinx: the database is at authz version %d, below %d heard of", p.Version, s.version)
	}
	return nil
}

// setUsers reads each of uids and sets it in st; false when a read met a
// policy other than st's (a policy write committed since: the policy must
// be loaded again).
func (e *engine) setUsers(ctx context.Context, st *state, uids []int64) (bool, error) {
	for _, uid := range uids {
		ur, err := e.l.UserRoles(ctx, uid)
		if err != nil {
			return false, err
		}
		if ur.PolicyVersion != st.policy {
			return false, nil
		}
		if err := st.setUser(uid, ur); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ------------------------------------------------------------ the state

// build loads p into a new enforcer: the p rows of active actions and
// every SuperKey row, the self permissions (self, the keys of
// authz.SelfPermissionKeys: this package cannot import authz) as
// SelfRole's p rows, and the g rows of the active users, each also linked
// to SelfRole.
func build(p Policy, self []string) (*state, error) {
	ps, err := policyRows(p, self)
	if err != nil {
		return nil, err
	}
	gs, users := linkRows(p)
	dc := &decisions{}
	e, err := newEnforcer(dc)
	if err != nil {
		return nil, err
	}
	m := e.GetModel()
	if len(ps) > 0 {
		if err := m.AddPolicies("p", "p", ps); err != nil {
			return nil, err
		}
	}
	if len(gs) > 0 {
		if err := m.AddPolicies("g", "g", gs); err != nil {
			return nil, err
		}
	}
	if err := e.BuildRoleLinks(); err != nil {
		return nil, err
	}
	st := &state{policy: p.PolicyVersion, e: e, dc: dc, users: users}
	st.version.Store(p.Version)
	return st, nil
}

// policyRows are the p rows build loads: those loadable, and SelfRole's.
func policyRows(p Policy, self []string) ([][]string, error) {
	ps := make([][]string, 0, len(p.Rules)+len(self))
	for _, r := range p.Rules {
		if loadable(r, p.Active) {
			ps = append(ps, []string{r.V0, r.V1, r.V2})
		}
	}
	for _, k := range self {
		sp, err := perm.Parse(k)
		if err != nil {
			return nil, fmt.Errorf("casbinx: self permission %q: %w", k, err)
		}
		ps = append(ps, []string{SelfRole, string(sp.Action), scopeOf(sp)})
	}
	return ps, nil
}

// linkRows are the g rows build loads, and the users they are of: every
// active user, linked to its roles and to SelfRole. A g row of an inactive
// user is not loaded: such a user holds nothing.
func linkRows(p Policy) ([][]string, map[int64]int64) {
	users := make(map[int64]int64, len(p.Users))
	gs := make([][]string, 0, 2*len(p.Users))
	for _, id := range p.Users {
		users[id] = p.Version
		gs = append(gs, []string{casbinrule.UserSub(id), SelfRole})
	}
	for _, r := range p.Rules {
		if id, ok := casbinrule.UserOf(r.V0); ok && r.Ptype == "g" && linkable(r.V1) {
			if _, active := users[id]; active {
				gs = append(gs, []string{r.V0, r.V1})
			}
		}
	}
	return gs, users
}

// linkable reports whether role is a role subject a g row may link a user
// to: SelfRole comes from the user being active, never from a stored row.
func linkable(role string) bool { return role != "" && role != SelfRole }

// loadable reports whether r is a p row the enforcer holds: a superuser
// row, or a grant of an active action. Grants of disabled and removed
// actions stay stored and come back with their action. Not loaded:
//   - a row with an empty column: an empty scope must not read as every
//     row;
//   - a stored row of SelfRole (casbin_rule_shape and roles_key_reserved
//     refuse one; this holds even past them): every active user holds
//     SelfRole, so its grants would reach everyone, not a role's holders.
func loadable(r Rule, active map[string]bool) bool {
	if r.Ptype != "p" || r.V0 == "" || r.V1 == "" || r.V2 == "" || r.V0 == SelfRole {
		return false
	}
	return r.V1 == SuperKey || active[r.V1]
}

// scopeOf is p's scope column: its suffix, AllRows for every row.
func scopeOf(p perm.Pattern) string {
	if s := p.Suffix(); s != "" {
		return s
	}
	return AllRows
}

// setUser replaces uid's g rows in the enforcer with those of ur (none for
// an inactive user; SelfRole's link for an active one) and drops what was
// read from the old ones, unless a newer read of uid is in.
func (st *state) setUser(uid int64, ur UserRoles) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if v, ok := st.users[uid]; ok && v > ur.Version {
		return nil
	}
	sub := casbinrule.UserSub(uid)
	// The user's own links, one by one: Casbin removes a single row in
	// place, where a filtered removal scans every g row of every user.
	held, err := st.e.GetRolesForUser(sub)
	if err != nil {
		return err
	}
	for _, r := range held {
		if _, err := st.e.RemoveGroupingPolicy(sub, r); err != nil {
			return err
		}
	}
	if ur.Active {
		links := [][]string{{sub, SelfRole}}
		for _, r := range ur.Roles {
			if linkable(r) {
				links = append(links, []string{sub, r})
			}
		}
		if _, err := st.e.AddGroupingPolicies(links); err != nil {
			return err
		}
	}
	st.users[uid] = ur.Version
	st.dc.dropSubject(sub)
	if _, ok := st.lists.LoadAndDelete(uid); ok {
		st.nLists.Add(-1)
	}
	return nil
}

// listOf is the set of the grants rows lists (GetImplicitPermissionsForUser:
// subject, action, scope); a SuperKey row sets Superuser.
func listOf(rows [][]string) core.PermissionSet {
	set := core.PermissionSet{Keys: map[string]struct{}{}}
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		if r[1] == SuperKey {
			set.Superuser = true
			continue
		}
		set.Add(grantKey(r[1], r[2]))
	}
	return set
}

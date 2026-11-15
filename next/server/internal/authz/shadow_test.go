package authz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// failOnShadowMismatch fails t on every shadow mismatch s reports while t
// runs, the comparisons still running when it ends included (its cleanup
// waits for them, before the database goes); one reported after that is
// left to TestMain (ShadowCheck), as t can no longer fail.
func failOnShadowMismatch(t *testing.T, s *Service) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
	t.Cleanup(settle)
	fn := func(m ShadowMismatch) bool {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			t.Errorf("authz shadow mismatch: %+v", m)
		}
		return !done
	}
	s.shadow.onMismatch.Store(&fn)
}

// settle waits for the comparisons and samples in flight.
func settle() { shadowWork.wait() }

// fakeResolver answers Grants with the function given. Its matcher path is
// the embedded nil Resolver's: casbinx.Enforce on it panics (counted as a
// Casbin error), so the tests of Enforce use a real resolver.
type fakeResolver struct {
	casbinx.Resolver
	grants func(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	calls  atomic.Int64
	// prewarmed is the version of the last Prewarm.
	prewarmed atomic.Int64
}

func (f *fakeResolver) Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	f.calls.Add(1)
	return f.grants(ctx, uid, min)
}

func (f *fakeResolver) Prewarm(v int64) { f.prewarmed.Store(v) }

func (f *fakeResolver) Reread(context.Context, int64) error { return nil }

// shadowFixture is a service on a fresh database with one user, its Casbin
// side a fakeResolver that answers what the legacy loader does (change
// grants to differ), no sample taken, and its counters as they are.
func shadowFixture(t *testing.T, engine EngineMode) (*Service, *fakeResolver, int64) {
	t.Helper()
	t.Setenv(EngineEnv, "")
	s := newService(t, testutil.DB(t), nil, nil)
	uid := addUser(t, s, "u@x.com", RoleUser)
	f := &fakeResolver{}
	f.grants = func(ctx context.Context, uid, _ int64) (core.PermissionSet, error) {
		return s.legacyPermissionSet(ctx, uid)
	}
	s.casbin = f
	s.shadow.sample = func(float64) bool { return false }
	s.setEngine(engine)
	drop(s)
	return s, f, uid
}

// since is the counters of s minus base.
func since(s *Service, base ShadowStats) ShadowStats {
	st := s.shadow.stats()
	return ShadowStats{Compared: st.Compared - base.Compared, Skipped: st.Skipped - base.Skipped,
		Mismatches: st.Mismatches - base.Mismatches, Errors: st.Errors - base.Errors, Dropped: st.Dropped - base.Dropped,
		EnforceChecked: st.EnforceChecked - base.EnforceChecked, EnforceMismatches: st.EnforceMismatches - base.EnforceMismatches}
}

// recordMismatches makes s collect its mismatches instead of failing.
func recordMismatches(s *Service) func() []ShadowMismatch {
	var mu sync.Mutex
	var got []ShadowMismatch
	fn := func(m ShadowMismatch) bool {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		return true
	}
	s.shadow.onMismatch.Store(&fn)
	return func() []ShadowMismatch {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// drop forgets the cached sets, so the next check loads again at the same
// version.
func drop(s *Service) {
	s.mu.Lock()
	clear(s.users)
	s.mu.Unlock()
}

// withExtra is f's legacy answer plus key, at the legacy version + skew.
func withExtra(s *Service, key string, skew int64) func(context.Context, int64, int64) (core.PermissionSet, error) {
	return func(ctx context.Context, uid, _ int64) (core.PermissionSet, error) {
		set, err := s.legacyPermissionSet(ctx, uid)
		set.Add(key)
		set.Version += skew
		return set, err
	}
}

func TestParseEngine(t *testing.T) {
	for in, want := range map[string]EngineMode{"legacy": EngineLegacy, "shadow": EngineShadow, "casbin": EngineCasbin,
		"": EngineShadow, "Casbin": EngineShadow, "casbin-only": EngineShadow, " legacy": EngineShadow} {
		if got := ParseEngine(in); got != want {
			t.Errorf("ParseEngine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEngineEnvOverride(t *testing.T) {
	for env, want := range map[string]EngineMode{"legacy": EngineLegacy, "casbin": EngineCasbin, "bogus": EngineShadow} {
		t.Setenv(EngineEnv, env)
		s := New(Deps{DB: &store.DB{}})
		if s.EngineMode() != want || !s.engineEnv {
			t.Errorf("%s=%s: engine %q (pinned %v), want %q", EngineEnv, env, s.EngineMode(), s.engineEnv, want)
		}
	}
	t.Setenv(EngineEnv, "")
	if s := New(Deps{DB: &store.DB{}}); s.EngineMode() != EngineShadow || s.engineEnv {
		t.Errorf("default engine %q (pinned %v), want shadow", s.EngineMode(), s.engineEnv)
	}
	t.Setenv(EngineEnv, "casbin")
	if s := New(Deps{}); s.EngineMode() != EngineLegacy {
		t.Errorf("engine without a database = %q, want legacy", s.EngineMode())
	}
}

func TestEngineLegacyDoesNotAskCasbin(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineLegacy)
	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
		t.Error("Casbin asked in legacy")
		return core.PermissionSet{}, nil
	}
	s.shadow.sample = func(float64) bool { return true }
	base := s.shadow.stats()
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	if st := since(s, base); st != (ShadowStats{}) {
		t.Fatalf("legacy counted %+v", st)
	}
}

func TestShadowComparesAndAnswersLegacy(t *testing.T) {
	ctx := context.Background()
	s, f, uid := shadowFixture(t, EngineShadow)
	base := s.shadow.stats()
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	if st := since(s, base); st.Compared != 1 || st.Mismatches != 0 {
		t.Fatalf("equal sets: %+v", st)
	}

	// A difference is reported once per user and version, and never
	// answers: legacy says no to user:read.
	got := recordMismatches(s)
	f.grants = withExtra(s, "user:read", 0)
	for range 3 {
		drop(s)
		mustCan(t, s, uid, "user:read", false)
		settle()
	}
	ms := got()
	if len(ms) != 1 || ms[0].Kind != MismatchSet || ms[0].UserID != uid || !slices.Equal(ms[0].CasbinOnly, []string{"user:read"}) || len(ms[0].LegacyOnly) != 0 {
		t.Fatalf("mismatches %+v", ms)
	}
	if st := since(s, base); st.Compared != 4 || st.Mismatches != 1 {
		t.Fatalf("after the mismatch: %+v", st)
	}
	// A new version reports it again.
	s.Committed(ctx, bump(t, s))
	mustCan(t, s, uid, "user:read", false)
	settle()
	if n := len(got()); n != 2 {
		t.Fatalf("after a bump: %d mismatches", n)
	}

	// Sets read at different versions are skipped, not compared, and so is
	// a Casbin resolution that could not read at one version.
	f.grants = withExtra(s, "user:read", 1)
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	settle() // the comparison reads f.grants
	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
		return core.PermissionSet{}, fmt.Errorf("%w: test", casbinx.ErrSkew)
	}
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	if st := since(s, base); st.Skipped != 2 || st.Mismatches != 2 || st.Errors != 0 {
		t.Fatalf("version skew: %+v", st)
	}

	// A Casbin error, or panic, is counted and the legacy answer stands.
	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
		return core.PermissionSet{}, errors.New("boom")
	}
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) { panic("boom") }
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	if st := since(s, base); st.Errors != 2 {
		t.Fatalf("casbin error and panic: %+v", st)
	}
}

// The Casbin side never holds the request: a comparison that does not end
// leaves the legacy answer on time, and with every slot taken the next one
// is dropped and counted, not queued.
func TestShadowOffTheRequest(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineShadow)
	release := make(chan struct{})
	f.grants = func(ctx context.Context, uid, _ int64) (core.PermissionSet, error) {
		<-release
		return s.legacyPermissionSet(ctx, uid)
	}
	base := s.shadow.stats()
	start := time.Now()
	mustCan(t, s, uid, "gateway:use", true)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the request waited %v for the Casbin side", d)
	}
	for range cap(s.shadow.slots) - 1 {
		s.shadow.slots <- struct{}{}
	}
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	if st := since(s, base); st.Dropped != 1 || st.Compared != 0 {
		t.Fatalf("while full: %+v", st)
	}
	for range cap(s.shadow.slots) - 1 {
		<-s.shadow.slots
	}
	close(release)
	settle()
	if st := since(s, base); st.Compared != 1 || st.Dropped != 1 {
		t.Fatalf("after the release: %+v", st)
	}
}

// The node has Casbin build the snapshot of each version it learns of,
// off the request, unless it runs legacy.
func TestPrewarmOnNewVersion(t *testing.T) {
	ctx := context.Background()
	s, f, _ := shadowFixture(t, EngineCasbin)
	v := bump(t, s)
	s.Committed(ctx, v)
	if got := f.prewarmed.Load(); got != v {
		t.Fatalf("prewarmed %d, want %d", got, v)
	}
	s.setEngine(EngineLegacy)
	s.Committed(ctx, bump(t, s))
	if got := f.prewarmed.Load(); got != v {
		t.Fatalf("legacy prewarmed %d", got)
	}
	s.setEngine(EngineShadow) // the switch builds the current version
	if got := f.prewarmed.Load(); got != s.Version() {
		t.Fatalf("switch prewarmed %d, want %d", got, s.Version())
	}
}

func TestCasbinAnswersAndFailsClosed(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineCasbin)
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true) // Casbin's answer

	for _, err := range []error{errors.New("boom"), fmt.Errorf("%w: test", casbinx.ErrSkew)} {
		f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
			return core.PermissionSet{}, err
		}
		drop(s)
		if ok, got := s.Can(context.Background(), uid, "gateway:use"); got == nil || ok {
			t.Fatalf("Casbin error %v: Can = %v, %v; want an error", err, ok, got)
		}
	}

	// Switching back to legacy drops what Casbin answered.
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true)
	s.setEngine(EngineLegacy)
	mustCan(t, s, uid, "user:read", false)
}

// A Casbin load still running when the operator switches to legacy answers
// its own request but is not cached: the next check is legacy's.
func TestEngineSwitchDropsLoadInFlight(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineCasbin)
	entered, release := make(chan struct{}), make(chan struct{})
	extra := withExtra(s, "user:read", 0)
	f.grants = func(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
		close(entered)
		<-release
		return extra(ctx, uid, min)
	}
	done := make(chan bool)
	go func() {
		ok, _ := s.Can(context.Background(), uid, "user:read")
		done <- ok
	}()
	<-entered
	s.setEngine(EngineLegacy)
	close(release)
	if !<-done {
		t.Fatal("the request in flight did not get Casbin's answer")
	}
	mustCan(t, s, uid, "user:read", false)
	if f.calls.Load() != 1 {
		t.Fatalf("Casbin asked %d times", f.calls.Load())
	}
}

func TestCasbinSamplesLegacy(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineCasbin)
	got := recordMismatches(s)
	s.shadow.sample = func(rate float64) bool { return rate == legacySampleRate }
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true)
	settle()
	if ms := got(); len(ms) != 1 || ms[0].Kind != MismatchSet || !slices.Equal(ms[0].CasbinOnly, []string{"user:read"}) {
		t.Fatalf("legacy sample mismatches %+v", ms)
	}
}

// extraGrants is a Loader whose policy has extra p rows besides the
// database's: Casbin then holds what the legacy tables do not.
type extraGrants struct {
	casbinx.Loader
	extra []casbinx.Rule
}

func (l extraGrants) Policy(ctx context.Context) (casbinx.Policy, error) {
	p, err := l.Loader.Policy(ctx)
	for _, r := range l.extra {
		p.Rules = append(p.Rules, r)
		p.Active[r.V1] = true
	}
	return p, err
}

// disableFirst is a Loader that, before its first read of a user's roles,
// disables the user with a raw UPDATE that does not bump the version (as
// tests do): Casbin reads the user after the write, the request's legacy
// set was read before it.
type disableFirst struct {
	casbinx.Loader
	db   *store.DB
	once sync.Once
}

func (l *disableFirst) UserRoles(ctx context.Context, uid int64) (casbinx.UserRoles, error) {
	var err error
	l.once.Do(func() {
		_, err = l.db.SQL.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, uid)
	})
	if err != nil {
		return casbinx.UserRoles{}, err
	}
	return l.Loader.UserRoles(ctx, uid)
}

// A difference a write the version does not record makes between the
// request's read and Casbin's is checked against a fresh legacy read, not
// reported: on the set path and on the matcher path.
func TestShadowRereadsAfterUnrecordedWrite(t *testing.T) {
	s, _, uid := shadowFixture(t, EngineShadow)
	l := &disableFirst{Loader: casbinx.NewLoader(s.db), db: s.db}
	s.casbin = casbinx.NewResolver(l, casbinx.NewRoleCache(l, nil), SelfPermissionKeys())
	failOnShadowMismatch(t, s)
	base := s.shadow.stats()
	mustCan(t, s, uid, "self:profile", true) // the legacy set, read active
	settle()
	if st := since(s, base); st.Compared != 1 || st.Mismatches != 0 || st.Errors != 0 {
		t.Fatalf("set path: %+v", st)
	}
	// The legacy set cached before the write still answers; Enforce reads
	// the disabled user.
	s.shadow.sample = func(rate float64) bool { return rate == enforceSampleRate }
	mustCan(t, s, uid, "self:profile", true)
	settle()
	if st := since(s, base); st.EnforceChecked != 1 || st.EnforceMismatches != 0 || st.Errors != 0 {
		t.Fatalf("matcher path: %+v", st)
	}
}

// casbin: the role cache holds the user read before a raw UPDATE disabled
// it; the sampled legacy read comes after. The difference is re-read past
// the role cache, not reported.
func TestCasbinSampleRereadsRoleCache(t *testing.T) {
	s, _, uid := shadowFixture(t, EngineCasbin)
	l := casbinx.NewLoader(s.db)
	s.casbin = casbinx.NewResolver(l, casbinx.NewRoleCache(l, nil), SelfPermissionKeys())
	failOnShadowMismatch(t, s)
	mustCan(t, s, uid, "self:profile", true) // the roles cached, active
	if _, err := s.db.SQL.ExecContext(context.Background(), `UPDATE users SET status = 'disabled' WHERE id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	drop(s)
	s.shadow.sample = func(rate float64) bool { return rate == legacySampleRate }
	base := s.shadow.stats()
	mustCan(t, s, uid, "self:profile", true) // Casbin answers from the cache
	settle()
	if st := since(s, base); st.Compared != 1 || st.Mismatches != 0 || st.Errors != 0 {
		t.Fatalf("legacy sample: %+v", st)
	}
	// The re-read replaced the cached entry: the next resolution sees it.
	drop(s)
	mustCan(t, s, uid, "self:profile", false)
}

// Sampled Can calls (shadow) are checked on the matcher path of a real
// resolver; here Casbin's policy grants the user role user:read.
func TestShadowSamplesEnforce(t *testing.T) {
	s, _, uid := shadowFixture(t, EngineShadow)
	l := extraGrants{casbinx.NewLoader(s.db), []casbinx.Rule{{Ptype: "p", V0: casbinx.RoleSub(RoleUser), V1: "user:read", V2: casbinx.AllRows}}}
	s.casbin = casbinx.NewResolver(l, casbinx.NewRoleCache(l, nil), SelfPermissionKeys())
	got := recordMismatches(s)
	s.shadow.sample = func(rate float64) bool { return rate == enforceSampleRate }
	base := s.shadow.stats()
	mustCan(t, s, uid, "gateway:use", true)
	settle()
	if st := since(s, base); st.EnforceChecked != 1 || st.EnforceMismatches != 0 {
		t.Fatalf("agreeing check: %+v", st)
	}
	mustCan(t, s, uid, "user:read", false)
	settle()
	var enforce []ShadowMismatch
	for _, m := range got() {
		if m.Kind == MismatchEnforce {
			enforce = append(enforce, m)
		}
	}
	if len(enforce) != 1 || enforce[0].Key != "user:read" || !slices.Equal(enforce[0].CasbinOnly, []string{"user:read"}) {
		t.Fatalf("enforce mismatches %+v", enforce)
	}
	if st := since(s, base); st.EnforceChecked != 2 || st.EnforceMismatches != 1 || st.Mismatches != 1 {
		t.Fatalf("enforce counters %+v", st)
	}

	// Only shadow samples the matcher path.
	s.setEngine(EngineLegacy)
	mustCan(t, s, uid, "user:update", false)
	settle()
	if st := since(s, base); st.EnforceChecked != 2 {
		t.Fatalf("legacy sampled Enforce: %+v", st)
	}
}

func TestDiffSetsNormalizes(t *testing.T) {
	a := core.PermissionSetOf("account:read", "account:read:group/12", "user:read")
	b := core.PermissionSetOf("account:read:group/12/*", "account:read:*", "user:read")
	if m, ok := diffSets(1, a, b); !ok {
		t.Fatalf("equal sets differ: %+v", m)
	}
	b = core.PermissionSetOf("account:read")
	b.Superuser = true
	m, ok := diffSets(1, a, b)
	if ok || !slices.Equal(m.LegacyOnly, []string{"account:read:group/12", "user:read"}) || !slices.Equal(m.CasbinOnly, []string{"*"}) {
		t.Fatalf("diff %+v", m)
	}
}

func TestEngineSetting(t *testing.T) {
	t.Setenv(EngineEnv, "")
	ctx := context.Background()
	db := testutil.DB(t)
	bus := newMemBus()
	a := newService(t, db, bus, nil)
	b := newService(t, db, bus, nil)
	root := addUser(t, a, "root@x.com", RoleSuperAdmin)
	admin := addUser(t, a, "admin@x.com", RoleAdmin)
	if st, err := a.EngineStatus(ctx); err != nil || st.Effective != EngineShadow || st.Source != "default" || st.Engine != "" {
		t.Fatalf("default status %+v, %v", st, err)
	}
	if _, err := a.SetEngine(ctx, root, "casbin-only"); fieldCode(err, "engine") != "invalid" {
		t.Fatalf("invalid engine: %v", err)
	}
	// Only the super admin switches, settings:manage or not.
	if _, err := a.SetEngine(ctx, admin, "legacy"); !isCode(err, "permission_denied") || a.EngineMode() != EngineShadow {
		t.Fatalf("admin switched the engine: %v", err)
	}
	st, err := a.SetEngine(ctx, root, "legacy")
	if err != nil || st.Effective != EngineLegacy || st.Source != "setting" || st.Engine != "legacy" {
		t.Fatalf("set legacy: %+v, %v", st, err)
	}
	var audited int
	if err := db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'authz.engine.set' AND user_id = ? AND target_id = 'authz'
		AND detail->>'from' = '' AND detail->>'to' = 'legacy'`, root).Row().Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit rows %d, %v", audited, err)
	}
	// The other node follows config:changed.
	eventually(t, func() bool { return b.EngineMode() == EngineLegacy })

	// A node that missed it follows the version poll; a stored value it
	// does not know is shadow.
	c := newService(t, db, nil, nil)
	if c.EngineMode() != EngineLegacy {
		t.Fatalf("started node: %q", c.EngineMode())
	}
	if err := db.Gorm.Exec(`UPDATE settings SET value = '{"engine":"Casbin"}' WHERE key = 'authz'`).Error; err != nil {
		t.Fatal(err)
	}
	var poll core.TaskSpec
	if err := c.RegisterTasks(taskRecorder(func(spec core.TaskSpec) {
		if spec.Name == VersionPollTask {
			poll = spec
		}
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := poll.Handler(ctx, core.TaskRun{Name: poll.Name}); err != nil {
		t.Fatal(err)
	}
	if c.EngineMode() != EngineShadow {
		t.Fatalf("after the poll: %q", c.EngineMode())
	}

	// EngineEnv pins a node whatever is stored.
	t.Setenv(EngineEnv, "casbin")
	d := newService(t, db, bus, nil)
	if st, err := d.SetEngine(ctx, root, "legacy"); err != nil || st.Effective != EngineCasbin || st.Source != "env" || st.Engine != "legacy" {
		t.Fatalf("pinned node: %+v, %v", st, err)
	}
}

// Switching the engine takes a step-up proof (authz.engine) besides
// settings:manage; reading it does not.
func TestEngineRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := httpapi.NewRouter(gin.New(), nil, New(Deps{}))
	New(Deps{}).registerEngineRoutes(r)
	want := map[string]string{http.MethodGet: "", http.MethodPut: core.StepUpAuthzEngine}
	for _, route := range r.Routes() {
		if route.Path != "/settings/authz" {
			continue
		}
		if scope, ok := want[route.Method]; !ok || route.StepUp != scope {
			t.Errorf("%s %s: step-up %q", route.Method, route.Path, route.StepUp)
		}
		delete(want, route.Method)
	}
	if len(want) != 0 {
		t.Fatalf("routes missing: %v", want)
	}
}

func TestRegisterTasksCasbinDrift(t *testing.T) {
	s := New(Deps{PollInterval: time.Second})
	var specs []core.TaskSpec
	if err := s.RegisterTasks(taskRecorder(func(spec core.TaskSpec) { specs = append(specs, spec) })); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(specs, func(s core.TaskSpec) bool { return s.Name == CasbinDriftTask })
	if i < 0 || specs[i].Scope != core.TaskCluster || specs[i].Every != 10*time.Minute || specs[i].Title["zh"] == "" {
		t.Fatalf("drift task %+v", specs)
	}
}

package authz

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// failOnShadowMismatch fails t on every shadow mismatch s reports while t
// runs; one found later (a sampled check still running) is left to
// TestMain (ShadowMismatchError), as t can no longer fail.
func failOnShadowMismatch(t *testing.T, s *Service) {
	var mu sync.Mutex
	done := false
	t.Cleanup(func() {
		mu.Lock()
		done = true
		mu.Unlock()
	})
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

// fakeResolver answers Grants and Enforce with the functions given.
type fakeResolver struct {
	grants  func(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	enforce func(uid int64, key string) (bool, error)
	calls   atomic.Int64
}

func (f *fakeResolver) Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error) {
	f.calls.Add(1)
	return f.grants(ctx, uid, min)
}

func (f *fakeResolver) Enforce(_ context.Context, uid int64, key string, _ int64) (bool, error) {
	return f.enforce(uid, key)
}

// shadowFixture is a service on a fresh database with one user, its Casbin
// side a fakeResolver that answers what the legacy loader does (change
// grants to differ), no sample taken, and its counters as they are.
func shadowFixture(t *testing.T, engine Engine) (*Service, *fakeResolver, int64) {
	t.Helper()
	t.Setenv(EngineEnv, "")
	s := newService(t, testutil.DB(t), nil, nil)
	uid := addUser(t, s, "u@x.com", RoleUser)
	f := &fakeResolver{enforce: func(int64, string) (bool, error) { return false, errors.New("unexpected Enforce") }}
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
		Mismatches: st.Mismatches - base.Mismatches, Errors: st.Errors - base.Errors,
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
	for in, want := range map[string]Engine{"legacy": EngineLegacy, "shadow": EngineShadow, "casbin": EngineCasbin,
		"": EngineShadow, "Casbin": EngineShadow, "casbin-only": EngineShadow, " legacy": EngineShadow} {
		if got := ParseEngine(in); got != want {
			t.Errorf("ParseEngine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEngineEnvOverride(t *testing.T) {
	for env, want := range map[string]Engine{"legacy": EngineLegacy, "casbin": EngineCasbin, "bogus": EngineShadow} {
		t.Setenv(EngineEnv, env)
		s := New(Deps{DB: &store.DB{}})
		if s.Engine() != want || !s.engineEnv {
			t.Errorf("%s=%s: engine %q (pinned %v), want %q", EngineEnv, env, s.Engine(), s.engineEnv, want)
		}
	}
	t.Setenv(EngineEnv, "")
	if s := New(Deps{DB: &store.DB{}}); s.Engine() != EngineShadow || s.engineEnv {
		t.Errorf("default engine %q (pinned %v), want shadow", s.Engine(), s.engineEnv)
	}
	if s := New(Deps{}); s.Engine() != EngineLegacy {
		t.Errorf("engine without a database = %q, want legacy", s.Engine())
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
	if st := since(s, base); st != (ShadowStats{}) {
		t.Fatalf("legacy counted %+v", st)
	}
}

func TestShadowComparesAndAnswersLegacy(t *testing.T) {
	ctx := context.Background()
	s, f, uid := shadowFixture(t, EngineShadow)
	base := s.shadow.stats()
	mustCan(t, s, uid, "gateway:use", true)
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
	if n := len(got()); n != 2 {
		t.Fatalf("after a bump: %d mismatches", n)
	}

	// Sets read at different versions are skipped, not compared.
	f.grants = withExtra(s, "user:read", 1)
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	if st := since(s, base); st.Skipped != 1 || st.Mismatches != 2 {
		t.Fatalf("version skew: %+v", st)
	}

	// A Casbin error is counted and the legacy answer stands.
	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
		return core.PermissionSet{}, errors.New("boom")
	}
	drop(s)
	mustCan(t, s, uid, "gateway:use", true)
	if st := since(s, base); st.Errors != 1 {
		t.Fatalf("casbin error: %+v", st)
	}
}

func TestCasbinAnswersAndFailsClosed(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineCasbin)
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true) // Casbin's answer

	f.grants = func(context.Context, int64, int64) (core.PermissionSet, error) {
		return core.PermissionSet{}, errors.New("boom")
	}
	drop(s)
	if ok, err := s.Can(context.Background(), uid, "gateway:use"); err == nil || ok {
		t.Fatalf("Casbin error: Can = %v, %v; want an error", ok, err)
	}

	// Switching back to legacy drops what Casbin answered.
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true)
	s.setEngine(EngineLegacy)
	mustCan(t, s, uid, "user:read", false)
}

func TestCasbinSamplesLegacy(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineCasbin)
	got := recordMismatches(s)
	s.shadow.sample = func(rate float64) bool { return rate == legacySampleRate }
	f.grants = withExtra(s, "user:read", 0)
	mustCan(t, s, uid, "user:read", true)
	eventually(t, func() bool { return len(got()) == 1 })
	if m := got()[0]; m.Kind != MismatchSet || !slices.Equal(m.CasbinOnly, []string{"user:read"}) {
		t.Fatalf("legacy sample mismatch %+v", m)
	}
}

func TestShadowSamplesEnforce(t *testing.T) {
	s, f, uid := shadowFixture(t, EngineShadow)
	got := recordMismatches(s)
	var asked sync.Map
	f.enforce = func(uid int64, key string) (bool, error) {
		asked.Store(key, true)
		return key == "gateway:use" || key == "user:read", nil
	}
	s.shadow.sample = func(rate float64) bool { return rate == enforceSampleRate }
	base := s.shadow.stats()
	mustCan(t, s, uid, "gateway:use", true)
	eventually(t, func() bool { return since(s, base).EnforceChecked == 1 })
	mustCan(t, s, uid, "user:read", false)
	eventually(t, func() bool { return len(got()) == 1 })
	m := got()[0]
	if m.Kind != MismatchEnforce || m.Key != "user:read" || !slices.Equal(m.CasbinOnly, []string{"user:read"}) {
		t.Fatalf("enforce mismatch %+v", m)
	}
	if st := since(s, base); st.EnforceChecked != 2 || st.EnforceMismatches != 1 || st.Mismatches != 0 {
		t.Fatalf("enforce counters %+v", st)
	}

	// Only shadow samples the matcher path.
	s.setEngine(EngineLegacy)
	asked.Clear()
	mustCan(t, s, uid, "user:update", false)
	if _, ok := asked.Load("user:update"); ok {
		t.Fatal("legacy sampled Enforce")
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
	if st, err := a.EngineStatus(ctx); err != nil || st.Effective != EngineShadow || st.Source != "default" || st.Engine != "" {
		t.Fatalf("default status %+v, %v", st, err)
	}
	if _, err := a.SetEngine(ctx, 0, "casbin-only"); fieldCode(err, "engine") != "invalid" {
		t.Fatalf("invalid engine: %v", err)
	}
	st, err := a.SetEngine(ctx, 0, "legacy")
	if err != nil || st.Effective != EngineLegacy || st.Source != "setting" || st.Engine != "legacy" {
		t.Fatalf("set legacy: %+v, %v", st, err)
	}
	// The other node follows config:changed.
	eventually(t, func() bool { return b.Engine() == EngineLegacy })

	// A node that missed it follows the version poll; a stored value it
	// does not know is shadow.
	c := newService(t, db, nil, nil)
	if c.Engine() != EngineLegacy {
		t.Fatalf("started node: %q", c.Engine())
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
	if c.Engine() != EngineShadow {
		t.Fatalf("after the poll: %q", c.Engine())
	}

	// EngineEnv pins a node whatever is stored.
	t.Setenv(EngineEnv, "casbin")
	d := newService(t, db, bus, nil)
	if st, err := d.SetEngine(ctx, 0, "legacy"); err != nil || st.Effective != EngineCasbin || st.Source != "env" || st.Engine != "legacy" {
		t.Fatalf("pinned node: %+v, %v", st, err)
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

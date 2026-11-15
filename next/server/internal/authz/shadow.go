package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

// The shadow comparison of the legacy loader and Casbin (CONTRACTS §99).
// This is the one file of package authz that calls casbinx.Enforce
// (gormguard [casbin-api]): Enforce runs every policy through the matcher
// and is never a request's answer, only a sampled check of it.
//
// Nothing here runs on the request: the comparisons and samples run on
// goroutines of their own, at most shadowConcurrency at once per node (one
// more is dropped and counted), each bounded by shadowTimeout, a panic in
// them counted as a Casbin error. The answer a request gets never waits for
// the side that does not give it.

// Sampling rates: in shadow, the share of Can calls whose answer the
// matcher path checks; in casbin, the share of Casbin sets the legacy loader
// recomputes. Test binaries take every one (newShadow).
const (
	enforceSampleRate = 0.01
	legacySampleRate  = 0.05
)

const (
	// shadowTimeout bounds one comparison or sample.
	shadowTimeout = 5 * time.Second
	// shadowConcurrency bounds the comparisons and samples in flight on a
	// node: after a version bump every active user misses at once, and the
	// side that does not answer must not take the database's connections
	// from the side that does.
	shadowConcurrency = 32
)

// dedupLimit bounds the mismatches remembered for deduplication; past it
// the memory starts over (a repeat is then logged once more).
const dedupLimit = 4096

// Mismatch kinds.
const (
	MismatchSet     = "set"     // the effective sets differ
	MismatchEnforce = "enforce" // Enforce disagrees with the set's Has for one key
)

// ShadowMismatch is one disagreement between the legacy loader and Casbin
// at one authz version: the grants (canonical keys, "*" for superuser) only
// one side holds. For an enforce mismatch Key is the key asked and the
// side that granted it lists it.
type ShadowMismatch struct {
	Kind       string
	UserID     int64
	Version    int64
	Key        string
	LegacyOnly []string
	CasbinOnly []string
}

// ShadowStats are this node's comparison counters since start.
type ShadowStats struct {
	// Compared sets, skipped ones (read at different versions), mismatched
	// ones (deduplicated by user and version), Casbin errors (panics
	// included) and the comparisons and samples dropped while
	// shadowConcurrency were running.
	Compared   int64 `json:"compared"`
	Skipped    int64 `json:"skipped_version_skew"`
	Mismatches int64 `json:"mismatches"`
	Errors     int64 `json:"errors"`
	Dropped    int64 `json:"dropped"`
	// Sampled Enforce checks (shadow) and their mismatches.
	EnforceChecked    int64 `json:"enforce_checked"`
	EnforceMismatches int64 `json:"enforce_mismatches"`
}

// The process-wide side of the comparison, for ShadowCheck: the work in
// flight on every Service, the mismatches no onMismatch handled, and the
// set comparisons started and made.
var (
	shadowWork      inflight
	shadowUnhandled atomic.Int64
	shadowStarted   atomic.Int64
	shadowCompared  atomic.Int64
)

// inflight counts the work in flight. Unlike a sync.WaitGroup, starting
// work while another goroutine waits is allowed: parallel tests each wait
// for the process-wide count (wait) while the others start comparisons.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle *sync.Cond
}

func (w *inflight) add() {
	w.mu.Lock()
	w.n++
	w.mu.Unlock()
}

func (w *inflight) done() {
	w.mu.Lock()
	if w.n--; w.n == 0 && w.idle != nil {
		w.idle.Broadcast()
	}
	w.mu.Unlock()
}

// wait returns once nothing is in flight.
func (w *inflight) wait() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.idle == nil {
		w.idle = sync.NewCond(&w.mu)
	}
	for w.n > 0 {
		w.idle.Wait()
	}
}

// ShadowCheck waits for the comparisons in flight on every Service of this
// process and reports what they found: a mismatch no test handled, or set
// comparisons started of which none could compare (all skipped as read at
// different versions, failed or dropped: a run that verified nothing). Test
// binaries call it after the run (TestMain), so that every test resolving
// permissions also checks Casbin against the legacy loader; nil without a
// finding.
func ShadowCheck() error {
	shadowWork.wait()
	var errs []error
	if n := shadowUnhandled.Load(); n > 0 {
		errs = append(errs, fmt.Errorf("authz shadow: %d mismatches between the legacy loader and Casbin (see the \"authz: shadow mismatch\" warnings)", n))
	}
	if n := shadowStarted.Load(); n > 0 && shadowCompared.Load() == 0 {
		errs = append(errs, fmt.Errorf("authz shadow: %d set comparisons started and none compared (version skew, errors or drops)", n))
	}
	return errors.Join(errs...)
}

// shadow holds the comparison state of a Service.
type shadow struct {
	compared, skipped, mismatches, errors, dropped atomic.Int64
	enforceChecked, enforceMismatches              atomic.Int64

	// sample reports whether to check this call at rate (set before the
	// service serves). onMismatch, when set, receives each new mismatch
	// after it is logged and reports whether it handled it (a test failed
	// or expected it); an unhandled one counts in ShadowCheck.
	sample     func(rate float64) bool
	onMismatch atomic.Pointer[func(ShadowMismatch) bool]
	// slots holds one token per comparison or sample in flight.
	slots chan struct{}

	mu       sync.Mutex
	seen     map[string]struct{}
	errorLog atomic.Int64 // unix seconds of the last logged Casbin error
}

func newShadow() *shadow {
	sample := func(rate float64) bool { return rand.Float64() < rate }
	if testing.Testing() {
		// A package's tests are what the comparison covers (ShadowCheck):
		// in them every check runs.
		sample = func(float64) bool { return true }
	}
	return &shadow{sample: sample, seen: map[string]struct{}{}, slots: make(chan struct{}, shadowConcurrency)}
}

func (sh *shadow) stats() ShadowStats {
	return ShadowStats{Compared: sh.compared.Load(), Skipped: sh.skipped.Load(), Mismatches: sh.mismatches.Load(),
		Errors: sh.errors.Load(), Dropped: sh.dropped.Load(), EnforceChecked: sh.enforceChecked.Load(),
		EnforceMismatches: sh.enforceMismatches.Load()}
}

// spawn runs fn for uid off the request: on a goroutine of its own, with a
// context of shadowTimeout detached from any request, while a slot is free
// (dropped and counted otherwise). Its error, or panic, counts as a Casbin
// error.
func (sh *shadow) spawn(uid int64, fn func(ctx context.Context) error) {
	select {
	case sh.slots <- struct{}{}:
	default:
		sh.dropped.Add(1)
		return
	}
	shadowWork.add()
	go func() {
		defer shadowWork.done()
		defer func() { <-sh.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), shadowTimeout)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				sh.failed(ctx, uid, fmt.Errorf("panic: %v", r))
			}
		}()
		if err := fn(ctx); err != nil {
			sh.failed(ctx, uid, err)
		}
	}()
}

// failed counts a Casbin error, logged at most once a minute: a broken
// Casbin side must not flood the log of a node the legacy loader serves.
func (sh *shadow) failed(ctx context.Context, uid int64, err error) {
	sh.errors.Add(1)
	now := time.Now().Unix()
	if last := sh.errorLog.Load(); now-last < 60 || !sh.errorLog.CompareAndSwap(last, now) {
		return
	}
	slog.WarnContext(ctx, "authz: shadow casbin error", "user_id", uid, "err", err)
}

// compareSets compares two sets of uid. Sets read at different versions
// are not compared (version skew): either may hold a change the other has
// not seen.
//
// The two sets are read one after the other (one on the request, the other
// off it), so a write the version does not record may land in between: the
// service's own writes all bump it, a raw UPDATE in a test does not. When
// they differ, again reads the side read first once more (after the other)
// and the fresh pair decides: a Casbin difference holds against it, a stale
// first read does not.
func (sh *shadow) compareSets(ctx context.Context, uid int64, legacy, casbin core.PermissionSet, again func(context.Context) (legacy, casbin core.PermissionSet, err error)) error {
	if legacy.Version != casbin.Version {
		sh.skipped.Add(1)
		return nil
	}
	m, ok := diffSets(uid, legacy, casbin)
	if !ok && again != nil {
		l, c, err := again(ctx)
		if err != nil {
			if sh.skew(err) {
				return nil
			}
			return err
		}
		return sh.compareSets(ctx, uid, l, c, nil)
	}
	sh.compared.Add(1)
	shadowCompared.Add(1)
	if !ok {
		sh.report(ctx, m)
	}
	return nil
}

// skew counts err as a version skew when Casbin could not read the policy
// and the roles at one version (casbinx.ErrSkew): nothing to compare, and
// not a failure.
func (sh *shadow) skew(err error) bool {
	if !errors.Is(err, casbinx.ErrSkew) {
		return false
	}
	sh.skipped.Add(1)
	return true
}

// report logs and counts m once per kind, user, version and key.
func (sh *shadow) report(ctx context.Context, m ShadowMismatch) {
	id := m.Kind + ":" + strconv.FormatInt(m.UserID, 10) + ":" + strconv.FormatInt(m.Version, 10) + ":" + m.Key
	sh.mu.Lock()
	_, dup := sh.seen[id]
	if !dup {
		if len(sh.seen) >= dedupLimit {
			clear(sh.seen)
		}
		sh.seen[id] = struct{}{}
	}
	sh.mu.Unlock()
	if dup {
		return
	}
	if m.Kind == MismatchEnforce {
		sh.enforceMismatches.Add(1)
	} else {
		sh.mismatches.Add(1)
	}
	slog.WarnContext(ctx, "authz: shadow mismatch", "kind", m.Kind, "user_id", m.UserID, "version", m.Version,
		"key", m.Key, "legacy_only", m.LegacyOnly, "casbin_only", m.CasbinOnly)
	if fn := sh.onMismatch.Load(); fn == nil || !(*fn)(m) {
		shadowUnhandled.Add(1)
	}
}

// diffSets compares the grants of two sets of uid (superuser flag, unscoped
// and scoped keys, in canonical form), ok when they are equal.
func diffSets(uid int64, legacy, casbin core.PermissionSet) (ShadowMismatch, bool) {
	l, c := grantList(legacy), grantList(casbin)
	m := ShadowMismatch{Kind: MismatchSet, UserID: uid, Version: legacy.Version, LegacyOnly: minus(l, c), CasbinOnly: minus(c, l)}
	return m, len(m.LegacyOnly) == 0 && len(m.CasbinOnly) == 0
}

// grantList is the normalized content of set: "*" for a superuser, then
// every canonical key it holds, scoped ones included even under an action
// it holds for every row (KeyList drops those), sorted.
func grantList(set core.PermissionSet) []string {
	var out []string
	if set.Superuser {
		out = append(out, casbinx.SuperKey)
	}
	for k := range set.Keys {
		out = append(out, k)
	}
	for a, scopes := range set.Scoped {
		for _, sc := range scopes {
			out = append(out, string(perm.Pattern{Action: a, Scope: sc}.Key()))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// minus lists the elements of a, sorted, that b (sorted) lacks.
func minus(a, b []string) []string {
	var out []string
	for _, x := range a {
		if _, found := slices.BinarySearch(b, x); !found {
			out = append(out, x)
		}
	}
	return out
}

// ------------------------------------------------------------ Service side

// shadowCompare resolves uid with Casbin at the version legacy was read at
// and compares (shadow, on a cache miss), off the request: nothing it finds
// changes the answer, legacy.
func (s *Service) shadowCompare(uid int64, legacy core.PermissionSet) {
	shadowStarted.Add(1)
	s.shadow.spawn(uid, func(ctx context.Context) error {
		cs, err := s.casbin.Grants(ctx, uid, legacy.Version)
		if err != nil {
			if s.shadow.skew(err) {
				return nil
			}
			return err
		}
		return s.shadow.compareSets(ctx, uid, legacy, cs, func(ctx context.Context) (core.PermissionSet, core.PermissionSet, error) {
			fresh, err := s.legacyPermissionSet(ctx, uid)
			return fresh, cs, err
		})
	})
}

// sampleLegacy recomputes a sampled Casbin set with the legacy loader
// (casbin), off the request.
func (s *Service) sampleLegacy(uid int64, cs core.PermissionSet) {
	if !s.shadow.sample(legacySampleRate) {
		return
	}
	shadowStarted.Add(1)
	s.shadow.spawn(uid, func(ctx context.Context) error {
		legacy, err := s.legacyPermissionSet(ctx, uid)
		if err != nil {
			return fmt.Errorf("legacy sample: %w", err)
		}
		return s.shadow.compareSets(ctx, uid, legacy, cs, func(ctx context.Context) (core.PermissionSet, core.PermissionSet, error) {
			fresh, err := s.casbin.Grants(ctx, uid, legacy.Version)
			return legacy, fresh, err
		})
	})
}

// sampleEnforce checks a sampled Can (shadow) on the matcher path, off the
// request: Enforce for key against the user's own set (the request's may be
// an access key's), when Casbin decides at the set's version.
func (s *Service) sampleEnforce(uid int64, key string) {
	if s.EngineMode() != EngineShadow || !s.shadow.sample(enforceSampleRate) {
		return
	}
	s.shadow.spawn(uid, func(ctx context.Context) error { return s.checkEnforce(ctx, uid, key) })
}

func (s *Service) checkEnforce(ctx context.Context, uid int64, key string) error {
	set, err := s.userPermissionSet(ctx, uid)
	if err != nil {
		return err
	}
	// Twice at most: a difference is checked again against a fresh legacy
	// read, made after Casbin's (see compareSets: the first set may come from
	// the cache, read before a write the version does not record).
	for fresh := false; ; fresh = true {
		got, v, err := casbinx.Enforce(ctx, s.casbin, uid, key, set.Version)
		if err != nil {
			if s.shadow.skew(err) {
				return nil
			}
			return err
		}
		if v != set.Version {
			s.shadow.skipped.Add(1)
			return nil
		}
		want := set.Has(key)
		if got != want && !fresh {
			if set, err = s.legacyPermissionSet(ctx, uid); err != nil {
				return err
			}
			continue
		}
		s.shadow.enforceChecked.Add(1)
		if got != want {
			m := ShadowMismatch{Kind: MismatchEnforce, UserID: uid, Version: set.Version, Key: key}
			if want {
				m.LegacyOnly = []string{key}
			} else {
				m.CasbinOnly = []string{key}
			}
			s.shadow.report(ctx, m)
		}
		return nil
	}
}

// ------------------------------------------------------------ verification

// EngineComparison is one user's sets as both engines resolve them
// (sub2api admin authz-verify).
type EngineComparison struct {
	Legacy, Casbin core.PermissionSet
	// Skew: read at different versions after a retry, not compared.
	Skew     bool
	Mismatch *ShadowMismatch
}

// CompareEngines resolves uid with both engines at one version (retrying
// once when a write moved it in between) and compares the sets. It records
// nothing: the caller reports.
func (s *Service) CompareEngines(ctx context.Context, uid int64) (EngineComparison, error) {
	if s.casbin == nil {
		return EngineComparison{}, errors.New("authz: no casbin resolver")
	}
	var c EngineComparison
	for range 2 {
		var err error
		if c.Legacy, err = s.legacyPermissionSet(ctx, uid); err != nil {
			return c, err
		}
		c.Casbin, err = s.casbin.Grants(ctx, uid, c.Legacy.Version)
		switch {
		case errors.Is(err, casbinx.ErrSkew):
			c.Skew = true
		case err != nil:
			return c, err
		default:
			c.Skew = c.Legacy.Version != c.Casbin.Version
		}
		if !c.Skew {
			break
		}
	}
	if !c.Skew {
		if m, ok := diffSets(uid, c.Legacy, c.Casbin); !ok {
			c.Mismatch = &m
		}
	}
	return c, nil
}

// Casbin is the Casbin resolver (nil without a database), for diagnostics.
func (s *Service) Casbin() casbinx.Resolver { return s.casbin }

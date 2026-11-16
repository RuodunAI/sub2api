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
// more is dropped and counted, or waits for a slot off the request with
// ShadowOptions.Queue), each bounded by shadowTimeout, a panic in them
// counted as a Casbin error. The answer a request gets never waits for the
// side that does not give it.

// Sampling rates: in shadow, the share of Can calls whose answer the
// matcher path checks; in casbin, the share of Casbin sets the legacy loader
// recomputes (ShadowOptions.Sample may take more).
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
	// staleLogEvery throttles the warnings of stale caches (each one is
	// counted): one cause, a write path that does not bump the version,
	// makes them for every user it writes.
	staleLogEvery = time.Minute
)

// dedupLimit bounds the mismatches remembered for deduplication; past it
// the memory starts over (a repeat is then logged once more).
const dedupLimit = 4096

// Mismatch kinds.
const (
	MismatchSet     = "set"     // the effective sets differ
	MismatchEnforce = "enforce" // Enforce disagrees with the set's Has for one key
)

// Sides of a stale cache (ShadowMismatch.Stale).
const (
	StaleLegacy = "legacy" // the legacy read changed when read again
	StaleCasbin = "casbin" // Casbin's answer changed past its role cache
	StaleBoth   = "both"
)

// ShadowMismatch is one disagreement between the legacy loader and Casbin
// at one authz version: the grants (canonical keys, "*" for superuser) only
// one side holds. For an enforce mismatch Key is the key asked and the
// side that granted it lists it.
//
// Stale marks a stale cache rather than a mismatch: the two sides
// disagreed as first read, and agreed at the same version once both were
// read again past the caches. The first read then answered from a state
// its version did not record (a write path that does not bump the version,
// a role cache entry of another state, a raw UPDATE in a test); Stale names
// the side whose answer changed, and the grants are the first read's.
type ShadowMismatch struct {
	Kind       string
	UserID     int64
	Version    int64
	Key        string
	LegacyOnly []string
	CasbinOnly []string
	Stale      string
}

// ShadowStats are this node's comparison counters since start.
type ShadowStats struct {
	// Compared sets, skipped ones (read at different versions), mismatched
	// ones and stale caches (each deduplicated by user and version), Casbin
	// errors (panics included) and the comparisons and samples dropped
	// while shadowConcurrency were running.
	Compared   int64 `json:"compared"`
	Skipped    int64 `json:"skipped_version_skew"`
	Mismatches int64 `json:"mismatches"`
	StaleCache int64 `json:"stale_cache"`
	Errors     int64 `json:"errors"`
	Dropped    int64 `json:"dropped"`
	// Sampled Enforce checks (shadow) and their mismatches.
	EnforceChecked    int64 `json:"enforce_checked"`
	EnforceMismatches int64 `json:"enforce_mismatches"`
	// The shares of the comparisons and samples attempted (compared,
	// checked, skipped, failed or dropped) that were skipped and dropped:
	// the part of the traffic the comparison did not cover (§99.5).
	SkippedRatio float64 `json:"skipped_ratio"`
	DroppedRatio float64 `json:"dropped_ratio"`
}

// ShadowOptions tune a Service's shadow comparison (Deps.Shadow); the zero
// value is production's.
type ShadowOptions struct {
	// Sample reports whether to check one call sampled at rate
	// (enforceSampleRate in shadow, legacySampleRate in casbin); nil draws
	// at random.
	Sample func(rate float64) bool
	// Queue makes a comparison or sample that finds shadowConcurrency
	// running wait for a slot, off the request, rather than be dropped.
	Queue bool
	// Observer, when set, follows the comparisons.
	Observer ShadowObserver
}

// ShadowObserver follows the shadow comparisons of a Service: tests count
// them, wait for them and fail on what they find (authz/authztest).
type ShadowObserver interface {
	// Event is told of each comparison or sample spawned off the request,
	// ended, dropped (every slot taken) or made (two answers compared).
	Event(ShadowEvent)
	// Found receives each new mismatch or stale cache after it is logged
	// and counted.
	Found(ShadowMismatch)
}

// ShadowEvent is a step of a comparison (ShadowObserver).
type ShadowEvent int

const (
	ShadowSpawned  ShadowEvent = iota // spawned off the request
	ShadowEnded                       // a spawned one ended
	ShadowDropped                     // dropped, every slot taken
	ShadowCompared                    // two answers compared (set or Enforce)
)

// shadow holds the comparison state of a Service.
type shadow struct {
	compared, skipped, mismatches, staleCache, errors, dropped atomic.Int64
	enforceChecked, enforceMismatches                          atomic.Int64

	// sample reports whether to check this call at rate; queue that a
	// comparison waits for a slot rather than be dropped; observer follows
	// them (nil: none; tests replace it).
	sample   func(rate float64) bool
	queue    bool
	observer atomic.Pointer[ShadowObserver]
	// slots holds one token per comparison or sample in flight.
	slots chan struct{}

	mu       sync.Mutex
	seen     map[string]struct{}
	errorLog atomic.Int64 // unix seconds of the last logged Casbin error
	staleLog atomic.Int64 // unix nanos of the last logged stale cache
}

func newShadow(o ShadowOptions) *shadow {
	sh := &shadow{sample: o.Sample, queue: o.Queue, seen: map[string]struct{}{}, slots: make(chan struct{}, shadowConcurrency)}
	if sh.sample == nil {
		sh.sample = func(rate float64) bool { return rand.Float64() < rate }
	}
	if o.Observer != nil {
		sh.observer.Store(&o.Observer)
	}
	return sh
}

func (sh *shadow) stats() ShadowStats {
	st := ShadowStats{Compared: sh.compared.Load(), Skipped: sh.skipped.Load(), Mismatches: sh.mismatches.Load(),
		StaleCache: sh.staleCache.Load(), Errors: sh.errors.Load(), Dropped: sh.dropped.Load(),
		EnforceChecked: sh.enforceChecked.Load(), EnforceMismatches: sh.enforceMismatches.Load()}
	if n := st.Compared + st.EnforceChecked + st.Skipped + st.Errors + st.Dropped; n > 0 {
		st.SkippedRatio = float64(st.Skipped) / float64(n)
		st.DroppedRatio = float64(st.Dropped) / float64(n)
	}
	return st
}

func (sh *shadow) event(e ShadowEvent) {
	if o := sh.observer.Load(); o != nil {
		(*o).Event(e)
	}
}

// spawn runs fn for uid off the request: on a goroutine of its own, with a
// context of shadowTimeout detached from any request, while a slot is free
// (dropped and counted otherwise, or waiting for one on that goroutine
// with queue). Its error, or panic, counts as a Casbin error.
func (sh *shadow) spawn(uid int64, fn func(ctx context.Context) error) {
	queue := sh.queue
	if !queue {
		select {
		case sh.slots <- struct{}{}:
		default:
			sh.dropped.Add(1)
			sh.event(ShadowDropped)
			return
		}
	}
	sh.event(ShadowSpawned)
	go func() {
		defer sh.event(ShadowEnded)
		if queue {
			sh.slots <- struct{}{}
		}
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

// made counts a comparison made: of two sets, or of Enforce with Has.
func (sh *shadow) made(enforce bool) {
	if enforce {
		sh.enforceChecked.Add(1)
	} else {
		sh.compared.Add(1)
	}
	sh.event(ShadowCompared)
}

// report logs and counts m once per kind, user, version, key and
// staleness; a stale cache is logged at most once per staleLogEvery.
func (sh *shadow) report(ctx context.Context, m ShadowMismatch) {
	id := m.Kind + ":" + strconv.FormatInt(m.UserID, 10) + ":" + strconv.FormatInt(m.Version, 10) + ":" + m.Key + ":" + m.Stale
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
	switch {
	case m.Stale != "":
		sh.staleCache.Add(1)
		if sh.logStale() {
			slog.WarnContext(ctx, "authz: shadow stale cache", "kind", m.Kind, "user_id", m.UserID, "version", m.Version,
				"key", m.Key, "stale", m.Stale, "legacy_only", m.LegacyOnly, "casbin_only", m.CasbinOnly)
		}
	case m.Kind == MismatchEnforce:
		sh.enforceMismatches.Add(1)
	default:
		sh.mismatches.Add(1)
	}
	if m.Stale == "" {
		slog.WarnContext(ctx, "authz: shadow mismatch", "kind", m.Kind, "user_id", m.UserID, "version", m.Version,
			"key", m.Key, "legacy_only", m.LegacyOnly, "casbin_only", m.CasbinOnly)
	}
	if o := sh.observer.Load(); o != nil {
		(*o).Found(m)
	}
}

// logStale reports whether a stale cache may be logged now
// (staleLogEvery).
func (sh *shadow) logStale() bool {
	now := time.Now().UnixNano()
	last := sh.staleLog.Load()
	return (last == 0 || now-last >= int64(staleLogEvery)) && sh.staleLog.CompareAndSwap(last, now)
}

// staleSide names the side whose answer changed between the first read
// and the second.
func staleSide(legacyChanged, casbinChanged bool) string {
	switch {
	case legacyChanged && casbinChanged:
		return StaleBoth
	case legacyChanged:
		return StaleLegacy
	}
	return StaleCasbin
}

// compareSets compares two sets of uid. Sets read at different versions
// are not compared (version skew): either may hold a change the other has
// not seen.
//
// When they differ, again reads both once more past the caches (the legacy
// loader's set from the database, Casbin's from roles read anew): still
// different, the fresh pair is the mismatch; equal at the same version, the
// first read answered from a state its version did not record, a stale
// cache (ShadowMismatch.Stale), counted apart and never passed off as
// agreement; it reports whether it found one. A fresh pair at another
// version decides nothing (skipped).
func (sh *shadow) compareSets(ctx context.Context, uid int64, legacy, casbin core.PermissionSet, again func(context.Context) (legacy, casbin core.PermissionSet, err error)) (stale bool, err error) {
	if legacy.Version != casbin.Version {
		sh.skipped.Add(1)
		return false, nil
	}
	m, ok := diffSets(uid, legacy, casbin)
	if ok {
		sh.made(false)
		return false, nil
	}
	l, c, err := again(ctx)
	switch {
	case err != nil && sh.skew(err):
		return false, nil
	case err != nil:
		return false, err
	case l.Version != legacy.Version || c.Version != legacy.Version:
		sh.skipped.Add(1)
		return false, nil
	}
	sh.made(false)
	if fresh, ok := diffSets(uid, l, c); !ok {
		sh.report(ctx, fresh)
		return false, nil
	}
	m.Stale = staleSide(!slices.Equal(grantList(legacy), grantList(l)), !slices.Equal(grantList(casbin), grantList(c)))
	sh.report(ctx, m)
	return true, nil
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
	s.shadow.spawn(uid, func(ctx context.Context) error {
		cs, err := s.casbin.Grants(ctx, uid, legacy.Version)
		if err != nil {
			if s.shadow.skew(err) {
				return nil
			}
			return err
		}
		return s.compare(ctx, uid, legacy, cs)
	})
}

// sampleLegacy recomputes a sampled Casbin set with the legacy loader
// (casbin), off the request.
func (s *Service) sampleLegacy(uid int64, cs core.PermissionSet) {
	if !s.shadow.sample(legacySampleRate) {
		return
	}
	s.shadow.spawn(uid, func(ctx context.Context) error {
		legacy, err := s.legacyPermissionSet(ctx, uid)
		if err != nil {
			return fmt.Errorf("legacy sample: %w", err)
		}
		return s.compare(ctx, uid, legacy, cs)
	})
}

// compare is shadow.compareSets with both sides read again past the caches
// (reread); after a stale cache the cached set (users) goes too.
func (s *Service) compare(ctx context.Context, uid int64, legacy, cs core.PermissionSet) error {
	stale, err := s.shadow.compareSets(ctx, uid, legacy, cs, s.reread(uid))
	if stale {
		s.forget(uid)
	}
	return err
}

// forget drops uid's cached set after a stale cache: whichever side it
// came from (legacy's answer in shadow, Casbin's in casbin), it was
// resolved from a state its version does not record, and the next check
// resolves it again.
func (s *Service) forget(uid int64) {
	s.mu.Lock()
	delete(s.users, uid)
	s.mu.Unlock()
}

// reread reads both sets of uid again past the caches (compareSets): the
// legacy loader's, and Casbin's from roles read anew (Reread, which keeps
// them in the role cache in place of the stale entry: the casbin engine
// must not go on answering from it), at the legacy read's version.
func (s *Service) reread(uid int64) func(context.Context) (core.PermissionSet, core.PermissionSet, error) {
	return func(ctx context.Context) (core.PermissionSet, core.PermissionSet, error) {
		if err := s.casbin.Reread(ctx, uid); err != nil {
			return core.PermissionSet{}, core.PermissionSet{}, err
		}
		legacy, err := s.legacyPermissionSet(ctx, uid)
		if err != nil {
			return core.PermissionSet{}, core.PermissionSet{}, err
		}
		cs, err := s.casbin.Grants(ctx, uid, legacy.Version)
		return legacy, cs, err
	}
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

// enforceAnswer is one side-by-side answer of Enforce and Has for a key.
type enforceAnswer struct {
	casbin, legacy bool
	version        int64
}

// checkEnforce compares Enforce with the cached set's Has, and a
// difference again against both sides read past the caches, as
// shadow.compareSets does for sets.
func (s *Service) checkEnforce(ctx context.Context, uid int64, key string) error {
	set, err := s.shadowSet(ctx, uid)
	if err != nil {
		return err
	}
	first, ok, err := s.enforceAt(ctx, uid, key, set)
	if !ok || err != nil {
		return err
	}
	if first.casbin == first.legacy {
		s.shadow.made(true)
		return nil
	}
	if err := s.casbin.Reread(ctx, uid); err != nil {
		return err
	}
	if set, err = s.legacyPermissionSet(ctx, uid); err != nil {
		return err
	}
	fresh, ok, err := s.enforceAt(ctx, uid, key, set)
	if !ok || err != nil {
		return err
	}
	if fresh.version != first.version {
		s.shadow.skipped.Add(1)
		return nil
	}
	s.shadow.made(true)
	if fresh.casbin != fresh.legacy {
		s.shadow.report(ctx, enforceMismatch(uid, key, fresh))
		return nil
	}
	m := enforceMismatch(uid, key, first)
	m.Stale = staleSide(fresh.legacy != first.legacy, fresh.casbin != first.casbin)
	s.shadow.report(ctx, m)
	s.forget(uid)
	return nil
}

// shadowSet is uid's cached set, or the legacy loader's read when none is
// cached: what Has answered with, without a comparison of its own (a
// sample must not spawn another and wait on the slots it holds).
func (s *Service) shadowSet(ctx context.Context, uid int64) (core.PermissionSet, error) {
	s.mu.RLock()
	set, ok := s.users[uid]
	min := s.minVersion
	s.mu.RUnlock()
	if ok && set.Version >= min {
		return set, nil
	}
	return s.legacyPermissionSet(ctx, uid)
}

// enforceAt asks Enforce for key at set's version; ok is false, counted as
// skipped, when Casbin decides at another version (or cannot read one).
func (s *Service) enforceAt(ctx context.Context, uid int64, key string, set core.PermissionSet) (a enforceAnswer, ok bool, err error) {
	got, v, err := casbinx.Enforce(ctx, s.casbin, uid, key, set.Version)
	switch {
	case err != nil && s.shadow.skew(err):
		return a, false, nil
	case err != nil:
		return a, false, err
	case v != set.Version:
		s.shadow.skipped.Add(1)
		return a, false, nil
	}
	return enforceAnswer{casbin: got, legacy: set.Has(key), version: v}, true, nil
}

// enforceMismatch is the mismatch of a, which disagree on key: the side
// that granted it lists it.
func enforceMismatch(uid int64, key string, a enforceAnswer) ShadowMismatch {
	m := ShadowMismatch{Kind: MismatchEnforce, UserID: uid, Version: a.version, Key: key}
	if a.legacy {
		m.LegacyOnly = []string{key}
	} else {
		m.CasbinOnly = []string{key}
	}
	return m
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

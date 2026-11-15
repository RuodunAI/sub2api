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
// This is the one file of package authz that calls Resolver.Enforce
// (gormguard [casbin-api]): Enforce runs every policy through the matcher
// and is never a request's answer, only a sampled check of it.

// Sampling rates: in shadow, the share of Can calls whose answer the
// matcher path checks; in casbin, the share of Casbin sets the legacy loader
// recomputes.
const (
	enforceSampleRate = 0.01
	legacySampleRate  = 0.05
)

// shadowTimeout bounds the Casbin side of a comparison on the request path
// (shadow, cache miss): Casbin must not hold a request the legacy loader
// already answered for longer. Sampled checks run off the request with
// shadowAsyncTimeout.
const (
	shadowTimeout      = time.Second
	shadowAsyncTimeout = 5 * time.Second
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
	// ones (deduplicated by user and version) and Casbin errors.
	Compared   int64 `json:"compared"`
	Skipped    int64 `json:"skipped_version_skew"`
	Mismatches int64 `json:"mismatches"`
	Errors     int64 `json:"errors"`
	// Sampled Enforce checks (shadow) and their mismatches.
	EnforceChecked    int64 `json:"enforce_checked"`
	EnforceMismatches int64 `json:"enforce_mismatches"`
}

// shadowMismatchTotal counts the mismatches of every Service of the
// process that no onMismatch handled, for ShadowMismatchError.
var shadowMismatchTotal atomic.Int64

// ShadowMismatchError reports the mismatches every Service of this process
// found, nil without one. Test binaries check it after the run (TestMain)
// so that every test resolving permissions in shadow, the default, also
// checks Casbin against the legacy loader.
func ShadowMismatchError() error {
	if n := shadowMismatchTotal.Load(); n > 0 {
		return fmt.Errorf("authz shadow: %d mismatches between the legacy loader and Casbin (see the \"authz: shadow mismatch\" warnings)", n)
	}
	return nil
}

// shadow holds the comparison state of a Service.
type shadow struct {
	compared, skipped, mismatches, errors atomic.Int64
	enforceChecked, enforceMismatches     atomic.Int64

	// sample reports whether to check this call at rate (set before the
	// service serves). onMismatch, when set, receives each new mismatch
	// after it is logged and reports whether it handled it (a test failed
	// or expected it); an unhandled one counts in ShadowMismatchError.
	sample     func(rate float64) bool
	onMismatch atomic.Pointer[func(ShadowMismatch) bool]

	mu       sync.Mutex
	seen     map[string]struct{}
	errorLog atomic.Int64 // unix seconds of the last logged Casbin error
}

func newShadow() *shadow {
	return &shadow{sample: func(rate float64) bool { return rand.Float64() < rate }, seen: map[string]struct{}{}}
}

func (sh *shadow) stats() ShadowStats {
	return ShadowStats{Compared: sh.compared.Load(), Skipped: sh.skipped.Load(), Mismatches: sh.mismatches.Load(),
		Errors: sh.errors.Load(), EnforceChecked: sh.enforceChecked.Load(), EnforceMismatches: sh.enforceMismatches.Load()}
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
func (sh *shadow) compareSets(ctx context.Context, uid int64, legacy, casbin core.PermissionSet) {
	if legacy.Version != casbin.Version {
		sh.skipped.Add(1)
		return
	}
	sh.compared.Add(1)
	if m, ok := diffSets(uid, legacy, casbin); !ok {
		sh.report(ctx, m)
	}
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
		shadowMismatchTotal.Add(1)
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

// casbinPermissionSet is uid's set as Casbin resolves it at this node's
// version or later. An error is returned, never answered around: in casbin
// the request fails closed.
func (s *Service) casbinPermissionSet(ctx context.Context, uid int64) (core.PermissionSet, error) {
	return s.casbin.Grants(ctx, uid, s.Version())
}

// shadowCompare resolves uid with Casbin at the version legacy was read at
// and compares (shadow, on a cache miss). Nothing it finds changes the
// answer, legacy.
func (s *Service) shadowCompare(ctx context.Context, uid int64, legacy core.PermissionSet) {
	ctx, cancel := context.WithTimeout(ctx, shadowTimeout)
	defer cancel()
	cs, err := s.casbin.Grants(ctx, uid, legacy.Version)
	if err != nil {
		s.shadow.failed(ctx, uid, err)
		return
	}
	s.shadow.compareSets(ctx, uid, legacy, cs)
}

// sampleLegacy recomputes a sampled Casbin set with the legacy loader
// (casbin), off the request.
func (s *Service) sampleLegacy(uid int64, cs core.PermissionSet) {
	if !s.shadow.sample(legacySampleRate) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shadowAsyncTimeout)
		defer cancel()
		legacy, err := s.legacyPermissionSet(ctx, uid)
		if err != nil {
			slog.WarnContext(ctx, "authz: legacy sample failed", "user_id", uid, "err", err)
			return
		}
		s.shadow.compareSets(ctx, uid, legacy, cs)
	}()
}

// sampleEnforce checks a sampled Can (shadow) on the matcher path, off the
// request: Enforce for key against the user's own set (the request's may be
// an access key's), when Casbin resolves the user at the same version.
func (s *Service) sampleEnforce(uid int64, key string) {
	if s.Engine() != EngineShadow || !s.shadow.sample(enforceSampleRate) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shadowAsyncTimeout)
		defer cancel()
		if err := s.checkEnforce(ctx, uid, key); err != nil {
			s.shadow.failed(ctx, uid, err)
		}
	}()
}

func (s *Service) checkEnforce(ctx context.Context, uid int64, key string) error {
	set, err := s.userPermissionSet(ctx, uid)
	if err != nil {
		return err
	}
	cs, err := s.casbin.Grants(ctx, uid, set.Version)
	if err != nil {
		return err
	}
	if cs.Version != set.Version {
		s.shadow.skipped.Add(1)
		return nil
	}
	got, err := s.casbin.Enforce(ctx, uid, key, set.Version)
	if err != nil {
		return err
	}
	s.shadow.enforceChecked.Add(1)
	if want := set.Has(key); got != want {
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
		if c.Casbin, err = s.casbin.Grants(ctx, uid, c.Legacy.Version); err != nil {
			return c, err
		}
		if c.Skew = c.Legacy.Version != c.Casbin.Version; !c.Skew {
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

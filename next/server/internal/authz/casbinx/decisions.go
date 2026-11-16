package casbinx

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/casbin/casbin/v3/persist/cache"
)

// Bounds of the decision cache: past them a decision is still made, just
// not kept. A user's keys come from code constants, the catalog and the
// rows it asks about, so a user rarely nears the first.
const (
	decisionsPerSubject = 4096
	decisionsTotal      = 1 << 20
)

// decisions is the enforcer's decision cache (SyncedCachedEnforcer.SetCache):
// Casbin's cache.Cache, keyed as Casbin keys a request ("<sub>$$<obj>$$"),
// held per subject so that one user's decisions drop alone (dropSubject)
// when that user's roles or status change. Reads take no lock.
type decisions struct {
	subs  sync.Map // subject -> *subjectDecisions
	total atomic.Int64
}

type subjectDecisions struct {
	m sync.Map // the rest of the key -> bool
	n atomic.Int64
}

var _ cache.Cache = (*decisions)(nil)

// splitKey splits a Casbin cache key at its first separator: the subject,
// and the rest.
func splitKey(key string) (sub, rest string) {
	sub, rest, _ = strings.Cut(key, "$$")
	return sub, rest
}

func (d *decisions) Get(key string) (bool, error) {
	sub, rest := splitKey(key)
	if s, ok := d.subs.Load(sub); ok {
		if v, ok := s.(*subjectDecisions).m.Load(rest); ok {
			return v.(bool), nil
		}
	}
	return false, cache.ErrNoSuchKey
}

// Set keeps a decision; extra (Casbin's expiry) is ignored: decisions drop
// with the policy or the user they were made on, never by age.
func (d *decisions) Set(key string, value bool, _ ...any) error {
	if d.total.Load() >= decisionsTotal {
		return nil
	}
	sub, rest := splitKey(key)
	v, _ := d.subs.LoadOrStore(sub, &subjectDecisions{})
	s := v.(*subjectDecisions)
	if s.n.Load() >= decisionsPerSubject {
		return nil
	}
	if _, loaded := s.m.Swap(rest, value); !loaded {
		s.n.Add(1)
		d.total.Add(1)
	}
	return nil
}

func (d *decisions) Delete(key string) error {
	sub, rest := splitKey(key)
	if s, ok := d.subs.Load(sub); ok {
		if _, loaded := s.(*subjectDecisions).m.LoadAndDelete(rest); loaded {
			s.(*subjectDecisions).n.Add(-1)
			d.total.Add(-1)
			return nil
		}
	}
	return cache.ErrNoSuchKey
}

func (d *decisions) Clear() error {
	d.subs.Range(func(sub, _ any) bool {
		d.dropSubject(sub.(string))
		return true
	})
	return nil
}

// dropSubject drops every decision made for sub.
func (d *decisions) dropSubject(sub string) {
	if s, ok := d.subs.LoadAndDelete(sub); ok {
		d.total.Add(-s.(*subjectDecisions).n.Load())
	}
}

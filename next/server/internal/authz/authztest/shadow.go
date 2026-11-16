// Package authztest runs the shadow comparison of the legacy loader and
// Casbin (CONTRACTS §99.5) over the tests of the packages that resolve
// permissions: services made with Shadow check every set they resolve and
// every Can, never drop a comparison, and report what they find to this
// process; TestMain fails the run on it (Check).
package authztest

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
)

// The process's comparisons: one test binary, every service made with
// Shadow.
var process collector

// collector counts the comparisons of the process and keeps the findings
// no test expected.
type collector struct {
	mu                         sync.Mutex
	idle                       *sync.Cond
	inFlight                   int
	spawned, compared, dropped int64
	unexpected                 []authz.ShadowMismatch
	unexpectedN                int
}

// keep bounds the unexpected findings Check prints.
const keep = 10

func (c *collector) event(e authz.ShadowEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e {
	case authz.ShadowSpawned:
		c.spawned++
		c.inFlight++
	case authz.ShadowEnded:
		if c.inFlight--; c.inFlight == 0 && c.idle != nil {
			c.idle.Broadcast()
		}
	case authz.ShadowDropped:
		c.dropped++
	case authz.ShadowCompared:
		c.compared++
	}
}

func (c *collector) found(m authz.ShadowMismatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unexpectedN++
	if len(c.unexpected) < keep {
		c.unexpected = append(c.unexpected, m)
	}
}

// wait returns once nothing is in flight. Unlike a sync.WaitGroup, starting
// work while another goroutine waits is allowed: parallel tests each wait
// for the process while the others start comparisons.
func (c *collector) wait() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle == nil {
		c.idle = sync.NewCond(&c.mu)
	}
	for c.inFlight > 0 {
		c.idle.Wait()
	}
}

// observer is a service's view of the collector: what its tests declared
// they expect.
type observer struct {
	acceptStale bool
}

func (o observer) Event(e authz.ShadowEvent) { process.event(e) }

func (o observer) Found(m authz.ShadowMismatch) {
	if m.Stale != "" && o.acceptStale {
		return
	}
	process.found(m)
}

// Option declares what a service's tests expect the comparison to find.
type Option func(*observer)

// AcceptStaleCache declares that the tests write the role tables without
// bumping the authz version (raw UPDATEs): a cached read of before such a
// write that the comparison then catches (a stale cache) is theirs, not a
// fault of the caches. Without it a stale cache fails the run.
func AcceptStaleCache(o *observer) { o.acceptStale = true }

// Shadow is the shadow comparison of a test service (authz.Deps.Shadow):
// every Can and every Casbin set checked, a comparison waiting for a slot
// rather than being dropped, all of it reported to Check.
func Shadow(opts ...Option) authz.ShadowOptions {
	var o observer
	for _, opt := range opts {
		opt(&o)
	}
	return authz.ShadowOptions{Sample: func(float64) bool { return true }, Queue: true, Observer: o}
}

// Wait returns once no comparison of the process is in flight (a test's
// cleanup, before its database goes).
func Wait() { process.wait() }

// Check waits for the comparisons in flight and reports what the run
// found: a mismatch or stale cache no test expected, a comparison dropped,
// or comparisons spawned of which none compared (all skipped as read at
// different versions, or failed: a run that verified nothing). TestMain
// calls it after the run (testutil.Exit); nil without a finding.
func Check() error {
	process.wait()
	process.mu.Lock()
	defer process.mu.Unlock()
	var errs []error
	if n := process.unexpectedN; n > 0 {
		errs = append(errs, fmt.Errorf("authz shadow: %d findings between the legacy loader and Casbin (the \"authz: shadow\" warnings), the first: %+v",
			n, process.unexpected))
	}
	if n := process.dropped; n > 0 {
		errs = append(errs, fmt.Errorf("authz shadow: %d comparisons dropped", n))
	}
	if process.spawned > 0 && process.compared == 0 {
		errs = append(errs, fmt.Errorf("authz shadow: %d comparisons spawned and none compared (version skew or errors)", process.spawned))
	}
	return errors.Join(errs...)
}

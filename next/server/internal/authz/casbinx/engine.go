package casbinx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// EngineOptions tunes Engine. Zero fields take the defaults.
type EngineOptions struct {
	// Wait bounds how long Ensure waits for a rebuild (default 2s); past
	// it Ensure fails (closed) rather than answer from an older snapshot.
	Wait time.Duration
	// Debounce is the least time between the starts of two rebuilds
	// (default 50ms): the version bumps of a burst are read by one rebuild.
	Debounce time.Duration
	// LoadTimeout bounds one rebuild (default 10s). A rebuild serves every
	// waiter, so it runs on its own context rather than the first caller's.
	LoadTimeout time.Duration
}

func (o EngineOptions) withDefaults() EngineOptions {
	if o.Wait <= 0 {
		o.Wait = 2 * time.Second
	}
	if o.Debounce <= 0 {
		o.Debounce = 50 * time.Millisecond
	}
	if o.LoadTimeout <= 0 {
		o.LoadTimeout = 10 * time.Second
	}
	return o
}

// ErrStale is Ensure's error when no snapshot of the version asked for
// could be had in time: the caller must not decide from an older one.
var ErrStale = errors.New("casbinx: no policy snapshot of the required version")

// Engine holds the current Snapshot and rebuilds it when a caller needs a
// newer version: one rebuild at a time (single flight), its waiters sharing
// it, and an older snapshot never replacing a newer one.
type Engine struct {
	build func(ctx context.Context) (*Snapshot, error)
	o     EngineOptions
	cur   atomic.Pointer[Snapshot]

	mu        sync.Mutex
	flight    *flight   // the rebuild in progress, nil when none
	lastStart time.Time // when the last rebuild started (Debounce)
}

// flight is one rebuild; done closes when it ends, err set before.
type flight struct {
	done chan struct{}
	err  error
}

// NewEngine returns an engine whose snapshots are built from l's policy
// with o; it holds none until the first Ensure.
func NewEngine(l Loader, o Options, eo EngineOptions) *Engine {
	return newEngine(func(ctx context.Context) (*Snapshot, error) {
		p, err := l.Policy(ctx)
		if err != nil {
			return nil, err
		}
		return Build(p, o)
	}, eo)
}

func newEngine(build func(context.Context) (*Snapshot, error), o EngineOptions) *Engine {
	return &Engine{build: build, o: o.withDefaults()}
}

// Current is the snapshot in place, nil before the first build.
func (g *Engine) Current() *Snapshot { return g.cur.Load() }

// Swap installs s unless a snapshot of the same or a newer version is in
// place: two rebuilds racing, the older one loses.
func (g *Engine) Swap(s *Snapshot) bool {
	for {
		old := g.cur.Load()
		if old != nil && old.version >= s.version {
			return false
		}
		if g.cur.CompareAndSwap(old, s) {
			return true
		}
	}
}

// Ensure returns a snapshot of version min or newer: the current one, or
// one it rebuilds for it. min is a committed version, so a rebuild that
// starts after the caller learnt of it reads min or newer; one that was
// already running may not, and Ensure then waits for one more. It fails
// with ErrStale when that one is still older, the rebuild's error when it
// fails, and ErrStale or ctx's error when Wait or ctx ends first.
func (g *Engine) Ensure(ctx context.Context, min int64) (*Snapshot, error) {
	if s := g.cur.Load(); s != nil && s.version >= min {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, g.o.Wait)
	defer cancel()
	for range 2 {
		f := g.rebuild()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: waiting for version %d: %w", ErrStale, min, ctx.Err())
		}
		if f.err != nil {
			return nil, f.err
		}
		if s := g.cur.Load(); s != nil && s.version >= min {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: version %d not reached", ErrStale, min)
}

// rebuild joins the rebuild in progress, or starts one.
func (g *Engine) rebuild() *flight {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.flight != nil {
		return g.flight
	}
	f := &flight{done: make(chan struct{})}
	g.flight = f
	delay := time.Until(g.lastStart.Add(g.o.Debounce))
	go g.run(f, delay)
	return f
}

// run is one rebuild: after delay (Debounce), read and build, and swap the
// snapshot in. Callers arriving during the delay join it.
func (g *Engine) run(f *flight, delay time.Duration) {
	defer func() {
		// The rebuild runs on its own goroutine: a panic in it would end
		// the process, not one request.
		if r := recover(); r != nil {
			f.err = fmt.Errorf("casbinx: rebuild the policy snapshot: panic: %v", r)
		}
		g.mu.Lock()
		g.flight = nil
		g.mu.Unlock()
		close(f.done)
	}()
	if delay > 0 {
		time.Sleep(delay)
	}
	g.mu.Lock()
	g.lastStart = time.Now()
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), g.o.LoadTimeout)
	defer cancel()
	s, err := g.build(ctx)
	if err != nil {
		f.err = fmt.Errorf("casbinx: rebuild the policy snapshot: %w", err)
		return
	}
	g.Swap(s)
}

package casbinx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLoader serves Policy at the version next returns, after delay, and
// counts and timestamps the reads. UserRoles reads users.
type fakeLoader struct {
	next   func(n int64) int64 // version of the n-th read (from 1)
	delay  time.Duration
	block  chan struct{} // when set, Policy waits for it to close
	err    error
	users  map[int64]UserRoles
	reads  atomic.Int64
	mu     sync.Mutex
	starts []time.Time
}

func (f *fakeLoader) Policy(ctx context.Context) (Policy, error) {
	n := f.reads.Add(1)
	f.mu.Lock()
	f.starts = append(f.starts, time.Now())
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Policy{}, ctx.Err()
		}
	}
	time.Sleep(f.delay)
	if f.err != nil {
		return Policy{}, f.err
	}
	v := n
	if f.next != nil {
		v = f.next(n)
	}
	rules := []Rule{grant("r:t", "account:read")}
	return Policy{Version: v, Active: activeOf(rules), Rules: rules}, nil
}

func (f *fakeLoader) UserRoles(_ context.Context, uid int64) (UserRoles, error) {
	return f.users[uid], nil
}

func fixed(v int64) func(int64) int64 { return func(int64) int64 { return v } }

func testEngine(l *fakeLoader, o EngineOptions) *Engine {
	return NewEngine(l, Options{Roles: newMapRoles()}, o)
}

// A snapshot at or past min is returned without a read.
func TestEnsureHit(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: fixed(5)}
	eng := testEngine(l, EngineOptions{})
	for _, min := range []int64{5, 1, 4, 5} {
		s, err := eng.Ensure(context.Background(), min)
		if err != nil || s.Version() != 5 {
			t.Fatalf("Ensure(%d) = %v, %v", min, s, err)
		}
	}
	if n := l.reads.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
}

// Concurrent callers share one rebuild.
func TestEnsureSingleFlight(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: fixed(7), delay: 20 * time.Millisecond}
	eng := testEngine(l, EngineOptions{})
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if s, err := eng.Ensure(context.Background(), 7); err != nil || s.Version() != 7 {
				t.Error(s, err)
			}
		})
	}
	wg.Wait()
	if n := l.reads.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
}

// Rebuilds start Debounce apart, and the callers of a burst arriving within
// it share the next rebuild.
func TestEnsureDebounce(t *testing.T) {
	t.Parallel()
	const debounce = 80 * time.Millisecond
	l := &fakeLoader{} // version n at the n-th read
	eng := testEngine(l, EngineOptions{Debounce: debounce})
	if _, err := eng.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := eng.Ensure(context.Background(), 2); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := l.reads.Load(); n != 2 {
		t.Fatalf("%d reads, want 2", n)
	}
	if gap := l.starts[1].Sub(l.starts[0]); gap < debounce {
		t.Fatalf("rebuilds %v apart, debounce %v", gap, debounce)
	}
}

// A rebuild that does not end within Wait fails the caller (closed): no
// snapshot, and the older one is not used.
func TestEnsureTimeoutFailsClosed(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: fixed(1)}
	eng := testEngine(l, EngineOptions{Wait: 50 * time.Millisecond})
	if _, err := eng.Ensure(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	l.block = make(chan struct{})
	defer close(l.block)
	start := time.Now()
	s, err := eng.Ensure(context.Background(), 2)
	if !errors.Is(err, ErrStale) || s != nil {
		t.Fatalf("Ensure = %v, %v; want ErrStale", s, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %v", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := eng.Ensure(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// A rebuild that read an older version than min (it started before min was
// committed) is followed by one more; still older, Ensure fails.
func TestEnsureRetriesOnceWhenBehind(t *testing.T) {
	t.Parallel()
	l := &fakeLoader{next: func(n int64) int64 { return []int64{1, 3}[min(n, 2)-1] }}
	eng := testEngine(l, EngineOptions{Debounce: time.Millisecond})
	if s, err := eng.Ensure(context.Background(), 3); err != nil || s.Version() != 3 {
		t.Fatalf("Ensure = %v, %v", s, err)
	}
	if n := l.reads.Load(); n != 2 {
		t.Fatalf("%d reads, want 2", n)
	}

	behind := &fakeLoader{next: fixed(1)}
	eng = testEngine(behind, EngineOptions{Debounce: time.Millisecond})
	if _, err := eng.Ensure(context.Background(), 2); !errors.Is(err, ErrStale) {
		t.Fatalf("Ensure = %v, want ErrStale", err)
	}
	if n := behind.reads.Load(); n != 2 {
		t.Fatalf("%d reads, want 2", n)
	}
}

// A failed rebuild fails its callers; the next Ensure tries again.
func TestEnsureLoadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	l := &fakeLoader{next: fixed(1), err: boom}
	eng := testEngine(l, EngineOptions{Debounce: time.Millisecond})
	if _, err := eng.Ensure(context.Background(), 1); !errors.Is(err, boom) {
		t.Fatalf("Ensure = %v", err)
	}
	if eng.Current() != nil {
		t.Fatal("a snapshot after a failed rebuild")
	}
	l.err = nil
	if s, err := eng.Ensure(context.Background(), 1); err != nil || s.Version() != 1 {
		t.Fatalf("Ensure = %v, %v", s, err)
	}
}

// A panic in a rebuild fails its callers instead of the process.
func TestEnsureRebuildPanics(t *testing.T) {
	t.Parallel()
	eng := newEngine(func(context.Context) (*Snapshot, error) { panic("boom") }, EngineOptions{})
	if _, err := eng.Ensure(context.Background(), 1); err == nil {
		t.Fatal("no error")
	}
}

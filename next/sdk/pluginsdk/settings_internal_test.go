package pluginsdk

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRead answers load with raw or err and counts the reads; gate, when
// set, holds every read until it is closed.
type fakeRead struct {
	n    atomic.Int32
	mu   sync.Mutex
	raw  string
	err  error
	gate chan struct{}
}

func (f *fakeRead) read(context.Context) ([]byte, error) {
	f.n.Add(1)
	f.mu.Lock()
	raw, err, gate := f.raw, f.err, f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

func (f *fakeRead) set(raw string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw, f.err = raw, err
}

func mustLoad(t *testing.T, c *settingsCache, f *fakeRead, want string) {
	t.Helper()
	raw, err := c.load(context.Background(), "sticky", f.read)
	if err != nil || string(raw) != want {
		t.Fatalf("load: %q %v, want %q", raw, err, want)
	}
}

// A fresh section is not read again; an expired or dropped one is.
func TestSettingsCacheExpiryAndDrop(t *testing.T) {
	var c settingsCache
	f := &fakeRead{raw: `{"a":1}`}
	mustLoad(t, &c, f, `{"a":1}`)
	mustLoad(t, &c, f, `{"a":1}`)
	if f.n.Load() != 1 {
		t.Fatalf("reads %d", f.n.Load())
	}
	c.entries["sticky"].at = time.Now().Add(-settingsTTL)
	f.set(`{"a":2}`, nil)
	mustLoad(t, &c, f, `{"a":2}`)
	c.drop([]string{"billing"})
	f.set(`{"a":3}`, nil)
	mustLoad(t, &c, f, `{"a":2}`) // another section changed
	c.drop([]string{"sticky"})
	mustLoad(t, &c, f, `{"a":3}`)
	c.drop(nil)
	f.set(`{"a":4}`, nil)
	mustLoad(t, &c, f, `{"a":4}`)
	if f.n.Load() != 4 {
		t.Fatalf("reads %d", f.n.Load())
	}

	var none *settingsCache
	none.drop(nil)
	if raw, err := none.load(context.Background(), "sticky", f.read); err != nil || string(raw) != `{"a":4}` || f.n.Load() != 5 {
		t.Fatalf("nil cache: %q %v", raw, err)
	}
}

// Concurrent callers share one read; a caller that gives up does not end
// the read for the others.
func TestSettingsCacheSharedRead(t *testing.T) {
	var c settingsCache
	f := &fakeRead{raw: `{}`, gate: make(chan struct{})}
	gone, cancel := context.WithCancel(context.Background())
	results := make(chan error, 9)
	go func() {
		_, err := c.load(gone, "sticky", f.read)
		results <- err
	}()
	for f.n.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	for range 8 {
		go func() {
			raw, err := c.load(context.Background(), "sticky", f.read)
			if err == nil && string(raw) != `{}` {
				err = errors.New("wrong values " + string(raw))
			}
			results <- err
		}()
	}
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the others are waiting for the read
	close(f.gate)
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if f.n.Load() != 1 {
		t.Fatalf("reads %d", f.n.Load())
	}
}

// A failed read keeps the last values, or its error when there are none,
// without asking the host again for settingsErrorTTL; a change asks at once.
// A read that started before a change is not kept.
func TestSettingsCacheFailure(t *testing.T) {
	var c settingsCache
	down := errors.New("host down")
	f := &fakeRead{err: down}
	if _, err := c.load(context.Background(), "sticky", f.read); !errors.Is(err, down) {
		t.Fatalf("no values yet: %v", err)
	}
	if _, err := c.load(context.Background(), "sticky", f.read); !errors.Is(err, down) || f.n.Load() != 1 {
		t.Fatalf("held back: %v, reads %d", err, f.n.Load())
	}
	f.set(`{"a":1}`, nil)
	c.drop([]string{"sticky"})
	mustLoad(t, &c, f, `{"a":1}`)

	f.set("", down)
	c.drop(nil)
	mustLoad(t, &c, f, `{"a":1}`) // the last values
	mustLoad(t, &c, f, `{"a":1}`)
	if f.n.Load() != 3 {
		t.Fatalf("reads %d", f.n.Load())
	}
	c.entries["sticky"].retryAt = time.Now()
	f.set(`{"a":2}`, nil)
	mustLoad(t, &c, f, `{"a":2}`)

	// A read in flight across a drop answers its callers but is not kept.
	f.set(`{"a":3}`, nil)
	f.gate = make(chan struct{})
	c.drop(nil)
	done := make(chan string)
	go func() {
		raw, _ := c.load(context.Background(), "sticky", f.read)
		done <- string(raw)
	}()
	for f.n.Load() != 5 {
		time.Sleep(time.Millisecond)
	}
	c.drop(nil)
	close(f.gate)
	if got := <-done; got != `{"a":3}` {
		t.Fatalf("read across a drop: %q", got)
	}
	f.gate = nil
	f.set(`{"a":4}`, nil)
	mustLoad(t, &c, f, `{"a":4}`)
}

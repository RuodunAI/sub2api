package grpcruntime

import (
	"context"
	"sync"
)

// byteBudget admits the request bodies handed to one plugin instance: a
// body joins those in flight - given to the plugin by Execute calls that
// have not returned - only while their sum stays within the limit of the
// moment. A body copied a few times in the plugin must fit its memory
// limit, and the core is the one that knows the sizes before it sends
// (docs DESIGN forwarding v2 §8.3). It budgets concurrency, not size: a
// body arriving alone is admitted whatever its size, so this never becomes
// a second request body limit.
type byteBudget struct {
	mu   sync.Mutex
	used int64
	wake chan struct{} // closed, and replaced, by every release
}

// acquire takes n bytes of a budget of limit bytes, waiting for releases
// until ctx ends. Nothing in flight always admits.
func (b *byteBudget) acquire(ctx context.Context, n, limit int64) bool {
	for {
		b.mu.Lock()
		if b.used == 0 || b.used+n <= limit {
			b.used += n
			b.mu.Unlock()
			return true
		}
		if b.wake == nil {
			b.wake = make(chan struct{})
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return false
		}
	}
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
}

func (b *byteBudget) inFlight() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

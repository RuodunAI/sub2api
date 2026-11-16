package blobs

import (
	"context"
	"errors"
	"testing"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// Without a configured limit transfers take the shared plugin package limit.
func TestLimitsDefaultToThePackageLimit(t *testing.T) {
	s := NewShell("unused.sock", "", 0)
	if s.max != 64<<20 || s.client.Timeout != rc.PluginPackageTransferTimeout {
		t.Fatalf("shell: max %d, timeout %s", s.max, s.client.Timeout)
	}
	if s = NewShell("unused.sock", "", 300<<20); s.max != 300<<20 {
		t.Fatalf("configured max: %d", s.max)
	}
	var limit int64
	src := &Source{Store: Dir(t.TempDir()), Download: func(_ context.Context, _ string, l int64) ([]byte, error) {
		limit = l
		return nil, errors.New("offline")
	}}
	_, _ = src.Fetch(context.Background(), sumOf([]byte("x")), "https://market/x")
	if limit != 64<<20 {
		t.Fatalf("download limit without MaxBytes: %d", limit)
	}
	src.MaxBytes = 300 << 20
	_, _ = src.Fetch(context.Background(), sumOf([]byte("x")), "https://market/x")
	if limit != 300<<20 {
		t.Fatalf("configured download limit: %d", limit)
	}
}

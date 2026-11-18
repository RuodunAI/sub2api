package app

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/install"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// fakeBuiltins answers BuiltinsReady with converged and signals every
// EnsureBuiltin pass, which the loop runs after a check that was not.
type fakeBuiltins struct {
	converged atomic.Bool
	ensured   chan struct{}
}

func (f *fakeBuiltins) BuiltinRequirements(string) (map[string]install.BuiltinRequirement, error) {
	return map[string]install.BuiltinRequirement{"volcengine": {Version: semver.MustParse("1.0.0")}}, nil
}

func (f *fakeBuiltins) BuiltinsReady(context.Context, map[string]install.BuiltinRequirement, core.Generation) (bool, error) {
	return f.converged.Load(), nil
}

func (f *fakeBuiltins) EnsureBuiltin(context.Context, string, *slog.Logger) error {
	select {
	case f.ensured <- struct{}{}:
	default:
	}
	return nil
}

type noRegistry struct{}

func (noRegistry) Current() core.Generation                       { return nil }
func (noRegistry) OnChange(func(core.Generation)) (cancel func()) { return func() {} }

// The bundled plugins gate a node's readiness at start only. Once they ran,
// a rollout of one of them (an install-only plugin being enabled reads as not
// converged on every node at once, from the shared plugin rows) must leave
// the node ready: /healthz answering 503 on both nodes together made the load
// balancer refuse every request while each node still served everything else.
func TestBuiltinReadinessSurvivesLaterRollouts(t *testing.T) {
	f := &fakeBuiltins{ensured: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready, stop := builtinConvergence(ctx, f, testutil.NewMemLocker(), noRegistry{}, "builtin", slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	defer stop(context.Background())

	waitEnsure := func() {
		t.Helper()
		select {
		case <-f.ensured:
		case <-time.After(30 * time.Second):
			t.Fatal("the loop did not try to install the bundled plugins")
		}
	}
	// Before the bundled plugins run here the node is not ready.
	waitEnsure()
	if ready.Load() {
		t.Fatal("ready before the bundled plugins ran")
	}
	f.converged.Store(true)
	deadline := time.Now().Add(30 * time.Second)
	for !ready.Load() {
		if time.Now().After(deadline) {
			t.Fatal("not ready after the bundled plugins ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Drop a pass signalled before the loop saw the plugins run.
	select {
	case <-f.ensured:
	default:
	}
	// A rollout of a bundled plugin starts: the loop converges again, the
	// node stays ready.
	f.converged.Store(false)
	waitEnsure()
	if !ready.Load() {
		t.Fatal("a rollout of a bundled plugin made the node unready")
	}
}

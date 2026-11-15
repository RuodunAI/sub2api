package dynweight

import (
	"context"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/plugins/dynweight/internal/rules"
)

// configVersion follows the saves, and prune deletes what is past every
// threshold and keeps the rest.
func TestStoreVersionAndPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := e.p.store(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.configVersion(ctx); v != 0 || err != nil {
		t.Fatalf("version before any save: %d %v", v, err)
	}
	e.configure(nil)
	e.configure(nil)
	if v, err := s.configVersion(ctx); v != 2 || err != nil {
		t.Fatalf("version after two saves: %d %v", v, err)
	}

	now := time.Now()
	old, fresh := key{AccountID: 1, GroupID: 1, Model: "m"}, key{AccountID: 2, GroupID: 1, Model: "m"}
	if err := s.addObs(ctx, []obsDelta{{key: old, window: 100}, {key: fresh, window: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveBases(ctx, map[pair]base{{1, 1}: {Weight: 5, SeenAt: now.Add(-time.Hour)}, {2, 1}: {Weight: 5, SeenAt: now}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []key{old, fresh} {
		if _, err := s.writeState(ctx, k, rules.State{WeightBase: 5, WeightEff: 5, LastEvalWindow: 100}, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.addEvents(ctx, []eventRow{{key: old}, {key: fresh}}); err != nil {
		t.Fatal(err)
	}
	// Counters before 150, events and pairs not seen in the last minute.
	n, err := s.prune(ctx, 150, now.Add(time.Minute), now.Add(-time.Minute), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 1 counter, 2 events, the state of old (its pair is stale), 1 base.
	if n != 5 {
		t.Fatalf("pruned %d rows", n)
	}
	states, err := s.loadStates(ctx, filter{})
	if _, ok := states[fresh]; err != nil || len(states) != 1 || !ok {
		t.Fatalf("states left %v %v", states, err)
	}
	bases, err := s.loadBases(ctx)
	if _, ok := bases[pair{2, 1}]; err != nil || len(bases) != 1 || !ok {
		t.Fatalf("bases left %v %v", bases, err)
	}
}

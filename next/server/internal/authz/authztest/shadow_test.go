package authztest

import (
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
)

// Check fails on a finding no test expected, a dropped comparison, and a
// run that compared nothing; AcceptStaleCache takes stale caches only.
func TestCheck(t *testing.T) {
	stale := authz.ShadowMismatch{Kind: authz.MismatchSet, UserID: 1, Version: 2, Stale: authz.StaleCasbin}
	mismatch := authz.ShadowMismatch{Kind: authz.MismatchSet, UserID: 1, Version: 2, CasbinOnly: []string{"user:read"}}
	for _, c := range []struct {
		name   string
		opts   []Option
		events []authz.ShadowEvent
		found  []authz.ShadowMismatch
		want   string
	}{
		{name: "clean", events: []authz.ShadowEvent{authz.ShadowSpawned, authz.ShadowCompared, authz.ShadowEnded}},
		{name: "stale", events: []authz.ShadowEvent{authz.ShadowCompared}, found: []authz.ShadowMismatch{stale}, want: "1 findings"},
		{name: "stale accepted", opts: []Option{AcceptStaleCache}, events: []authz.ShadowEvent{authz.ShadowCompared}, found: []authz.ShadowMismatch{stale}},
		{name: "mismatch, stale accepted", opts: []Option{AcceptStaleCache}, events: []authz.ShadowEvent{authz.ShadowCompared}, found: []authz.ShadowMismatch{mismatch}, want: "1 findings"},
		{name: "dropped", events: []authz.ShadowEvent{authz.ShadowCompared, authz.ShadowDropped}, want: "1 comparisons dropped"},
		{name: "nothing compared", events: []authz.ShadowEvent{authz.ShadowSpawned, authz.ShadowEnded}, want: "none compared"},
	} {
		t.Run(c.name, func(t *testing.T) {
			process = collector{}
			o := Shadow(c.opts...).Observer
			for _, e := range c.events {
				o.Event(e)
			}
			for _, m := range c.found {
				o.Found(m)
			}
			err := Check()
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("Check() = %v, want %q", err, c.want)
			}
		})
	}
	process = collector{}
}

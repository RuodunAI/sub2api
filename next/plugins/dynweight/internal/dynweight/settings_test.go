package dynweight

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"

	"github.com/Sub2API-Devs/sup2api/next/plugins/dynweight/internal/rules"
)

// While the core's sticky sessions are off a queued clear is given up with
// an event saying so, never run late; once they are back on clears run.
func TestStickyClearSkippedWhileStickyOff(t *testing.T) {
	e := newEnv(t)
	e.configure(nil)
	e.fh.SetSettings(protocol.StickySettings{Enabled: false})
	e.rank(100)
	e.window(5, 5)
	e.window(5, 5)
	if len(e.cleared) != 0 || len(e.pendingClears()) != 0 {
		t.Fatalf("sticky off: cleared %d, pending %+v", len(e.cleared), e.pendingClears())
	}
	ev := e.events(map[string]string{"kind": rules.EventAffinityClear})
	if len(ev) != 1 || ev[0].Detail["skipped"] != "sticky_disabled" || ev[0].Detail["cleared"] != nil {
		t.Fatalf("skipped event %+v", ev)
	}

	e.fh.SetSettings(protocol.StickySettings{Enabled: true})
	if err := e.h.SettingsChanged(protocol.SettingsSticky); err != nil {
		t.Fatal(err)
	}
	e.window(0, 0) // recovers
	e.window(5, 5)
	e.window(5, 5)
	if len(e.cleared) != 1 || len(e.pendingClears()) != 0 {
		t.Fatalf("sticky on: cleared %d, pending %+v", len(e.cleared), e.pendingClears())
	}
	ev = e.events(map[string]string{"kind": rules.EventAffinityClear})
	if len(ev) != 2 || ev[0].Detail["skipped"] != nil || ev[0].Detail["cleared"] != float64(2) {
		t.Fatalf("clear event %+v", ev)
	}
}

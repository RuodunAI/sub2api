package dynweight

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

// While the core's sticky sessions are off the queued clears wait; they run
// once the core says sticky sessions are back on.
func TestStickyClearWaitsWhileStickyOff(t *testing.T) {
	e := newEnv(t)
	e.configure(nil)
	e.fh.SetSettings(protocol.StickySettings{Enabled: false})
	e.rank(100)
	e.window(5, 5)
	e.window(5, 5)
	if len(e.cleared) != 0 || len(e.pendingClears()) != 1 {
		t.Fatalf("sticky off: cleared %d, pending %+v", len(e.cleared), e.pendingClears())
	}
	e.window(0, 0)
	if len(e.cleared) != 0 || len(e.pendingClears()) != 1 {
		t.Fatalf("still off: cleared %d, pending %+v", len(e.cleared), e.pendingClears())
	}

	e.fh.SetSettings(protocol.StickySettings{Enabled: true})
	if err := e.h.SettingsChanged(protocol.SettingsSticky); err != nil {
		t.Fatal(err)
	}
	e.window(0, 0)
	if len(e.cleared) != 1 || len(e.pendingClears()) != 0 {
		t.Fatalf("sticky on: cleared %d, pending %+v", len(e.cleared), e.pendingClears())
	}
}

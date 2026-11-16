package supervisor

import (
	"context"
	"testing"
	"time"

	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// recordingControl answers like a core that drains at once and records the
// drain request it received.
type recordingControl struct {
	got *rc.DrainRequest
}

func (c *recordingControl) Drain(_ context.Context, d rc.DrainRequest) error {
	c.got = &d
	return nil
}
func (c *recordingControl) Status(context.Context) (rc.Status, error) {
	return rc.Status{DrainComplete: true}, nil
}
func (c *recordingControl) Shutdown(context.Context) error { return nil }

func TestDrainStopSendsGracefulAsAsked(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		m := &Manager{} // not running: DrainStop only talks to the control socket and then Wait returns at once
		c := &recordingControl{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := m.DrainStop(ctx, c, "op", graceful)
		cancel()
		if err != nil {
			t.Fatalf("graceful=%v: %v", graceful, err)
		}
		if c.got == nil || c.got.Graceful != graceful || c.got.OperationID != "op" || c.got.Deadline.IsZero() {
			t.Fatalf("drain request %+v, want graceful=%v", c.got, graceful)
		}
	}
}

func TestDrainStopRequiresADeadline(t *testing.T) {
	m := &Manager{}
	c := &recordingControl{}
	if err := m.DrainStop(context.Background(), c, "op", true); err == nil || c.got != nil {
		t.Fatal("drain without a deadline was sent")
	}
}

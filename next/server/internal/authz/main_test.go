package authz_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain runs the package's services on authztest's shadow comparison
// and fails the run on what it found that no test caught itself (one a
// test made without newService, or found after its test ended).
func TestMain(m *testing.M) {
	authz.SetTestShadow(authztest.Shadow(), authztest.Wait)
	testutil.Exit(m.Run(), authztest.Check)
}

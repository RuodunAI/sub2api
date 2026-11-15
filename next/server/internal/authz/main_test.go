package authz

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run on a shadow mismatch no test caught itself (one a
// test made without newService, or found after its test ended).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), ShadowMismatchError) }

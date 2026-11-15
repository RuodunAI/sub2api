package casbinx_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run on a shadow mismatch between the legacy loader and
// Casbin in any test (authz.ShadowCheck).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), authz.ShadowCheck) }

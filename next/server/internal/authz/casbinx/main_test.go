package casbinx_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run on a shadow mismatch between the legacy loader and
// Casbin in the services its tests make with authztest.Shadow
// (authztest.Check).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), authztest.Check) }

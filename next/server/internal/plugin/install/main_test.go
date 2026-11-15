package install

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain removes registrytest's shared package directory after the run
// and fails it on what the shadow comparison of the legacy loader and
// Casbin found in any test (authz.ShadowCheck).
func TestMain(m *testing.M) { testutil.Exit(registrytest.Run(m), authz.ShadowCheck) }

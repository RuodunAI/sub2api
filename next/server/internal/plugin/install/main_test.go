package install

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
)

// TestMain removes registrytest's shared package directory after the run
// and fails it on a shadow mismatch between the legacy loader and Casbin
// (authz.ShadowMismatchError).
func TestMain(m *testing.M) { registrytest.Main(m, authz.ShadowMismatchError) }

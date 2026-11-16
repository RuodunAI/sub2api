package segment

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run on what the shadow comparison of the legacy
// loader and Casbin found in any test (authz.ShadowCheck: authz.engine
// shadow, the default; SUB2API_AUTHZ_ENGINE picks another).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), authz.ShadowCheck) }

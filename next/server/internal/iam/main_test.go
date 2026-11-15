package iam

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run when, in any test, Casbin resolved a permission
// set other than the legacy loader's (authz.engine shadow, the default;
// SUP2API_AUTHZ_ENGINE picks another).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), authz.ShadowMismatchError) }

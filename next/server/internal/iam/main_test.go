package iam

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// TestMain fails the run on what the shadow comparison of the legacy
// loader and Casbin found in the services its tests make with
// authztest.Shadow (authztest.Check: authz.engine shadow, the default;
// SUP2API_AUTHZ_ENGINE picks another).
func TestMain(m *testing.M) { testutil.Exit(m.Run(), authztest.Check) }

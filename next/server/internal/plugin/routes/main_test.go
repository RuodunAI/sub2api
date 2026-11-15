package routes_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry/registrytest"
)

// TestMain removes registrytest's shared package directory after the run.
func TestMain(m *testing.M) { registrytest.Main(m) }

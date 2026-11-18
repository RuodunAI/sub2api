package gemini

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// TestGolden replays the frozen answers of the plugin as it was released
// (all cases, no exceptions) against the platform main serves.
func TestGolden(t *testing.T) {
	goldentest.Run(t, "gemini", standardtest.Golden(New()))
}

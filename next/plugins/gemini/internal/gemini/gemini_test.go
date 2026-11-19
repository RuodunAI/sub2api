package gemini

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// TestGolden replays the frozen answers of the plugin as it was released
// against the platform main serves: all cases, except that it now refuses
// a key the account form never sends.
func TestGolden(t *testing.T) {
	standardtest.Run(t, "gemini", "../../manifest.json", New(),
		goldentest.Except("validate valid with other fields", "the account form's keys only: the others are refused"))
}

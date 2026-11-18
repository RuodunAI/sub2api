package gemini_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// The gemini plugin serves the preset as it is (it filters nothing).
func TestGolden(t *testing.T) {
	standardtest.Run(t, "gemini", standard.New(gemini.APIKey("apikey")))
}

package gemini_test

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/goldentest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard/standardtest"
)

// The gemini plugin serves the preset as it is (it filters nothing). The
// preset has no manifest: the plugin's own golden test checks its
// passHeaders.
func TestGolden(t *testing.T) {
	f, err := goldentest.Load("gemini")
	if err != nil {
		t.Fatal(err)
	}
	goldentest.Run(t, "gemini", standardtest.Golden(standard.New(gemini.APIKey("apikey")), f.PassHeaders))
}

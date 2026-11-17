// Command vertex is the sub2api-next Vertex AI plugin: account types for the
// built-in gemini platform that reach Gemini through Google Cloud Vertex AI
// (service account key, user credentials, API key).
package main

import (
	_ "embed"

	"github.com/Sub2API-Devs/sup2api/next/plugins/vertex/internal/vertex"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

//go:embed manifest.json
var manifestJSON []byte

func main() {
	pluginsdk.Serve(vertex.New(), pluginsdk.WithManifest(manifestJSON))
}

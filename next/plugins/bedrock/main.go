// Command bedrock is the sub2api-next AWS Bedrock plugin: account types for
// the built-in anthropic and openai platforms that reach the models through
// the Bedrock runtime (Bedrock API key, access key, temporary credentials).
package main

import (
	_ "embed"

	"github.com/Sub2API-Devs/sup2api/next/plugins/bedrock/internal/bedrock"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

//go:embed manifest.json
var manifestJSON []byte

func main() {
	pluginsdk.Serve(bedrock.New(), pluginsdk.WithManifest(manifestJSON))
}

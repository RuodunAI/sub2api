// Package typesafe is the platform of the TypeSafe API key account type:
// the SDK's typesafe preset as it is.
package typesafe

import (
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/typesafe"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// TypeAPIKey is the account type id (manifest.json accountTypes[0].id).
const TypeAPIKey = "apikey"

// New returns the platform of the apikey account type.
func New() *standard.Platform { return standard.New(typesafe.APIKey(TypeAPIKey)) }

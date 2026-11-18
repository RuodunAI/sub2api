package pluginsdk

import (
	"github.com/tidwall/gjson"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// RequestModel returns the model to send upstream: the "model" of the body
// when it is a non-empty string, else meta.model. The core has already
// applied the account's model mapping (CONTRACTS §18) to both.
func RequestModel(in *pluginv1.ExecuteRequest) string {
	if m := gjson.GetBytes(in.GetBody(), "model"); m.Type == gjson.String && m.Str != "" {
		return m.Str
	}
	return in.GetMeta().GetModel()
}

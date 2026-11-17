package pluginsdk

import (
	"encoding/json"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// RequestModel returns the model to send upstream: the body field when the
// core forwarded one (requestFields["model"]), else meta.model. The core has
// already applied the account's model mapping (CONTRACTS §18), so a plugin
// only has to prefer the body value over the metadata when both are present.
// A body value that is not a JSON string is ignored, like an empty one.
func RequestModel(in *pluginv1.BuildUpstreamRequestRequest) string {
	model := in.GetMeta().GetModel()
	if raw, ok := in.GetFields()["model"]; ok && raw != "" {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil && s != "" {
			model = s
		}
	}
	return model
}

// SetPatch is the body patch setting path to value (encoded as JSON). A
// value that cannot be encoded sets null.
func SetPatch(path string, value any) *pluginv1.BodyPatch {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = []byte("null")
	}
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_SET, Path: path, ValueJson: string(raw)}
}

// DeletePatch is the body patch removing path; removing an absent path
// changes nothing.
func DeletePatch(path string) *pluginv1.BodyPatch {
	return &pluginv1.BodyPatch{Op: pluginv1.BodyPatch_OP_DELETE, Path: path}
}

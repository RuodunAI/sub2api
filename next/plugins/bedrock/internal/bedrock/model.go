package bedrock

import (
	"net/url"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// arnPrefix starts a model named by its ARN (an application inference
// profile, a provisioned throughput, an imported model).
const arnPrefix = "arn:"

// govPrefix is the inference profile prefix of the GovCloud regions. It is
// no choice of the prefix setting: an administrator writes such ids whole.
const govPrefix = "us-gov"

// regionalPrefixes start an inference profile id, a model id that already
// says which geography serves it: every prefix an account can choose
// (ProfilePrefixes), and govPrefix.
var regionalPrefixes = func() []string {
	out := []string{govPrefix + "."}
	for _, prefix := range ProfilePrefixes {
		if prefix != NoProfilePrefix {
			out = append(out, prefix+".")
		}
	}
	return out
}()

// modelID is the Bedrock id the account invokes for model: the account's
// inference profile prefix goes before a plain model id, while an ARN and an
// id that already carries a prefix stay as they are.
func (a account) modelID(model string) (string, error) {
	id := strings.TrimSpace(model)
	switch {
	case id == "":
		return "", status.Error(codes.InvalidArgument, "the request names no model")
	case a.prefix == NoProfilePrefix || strings.HasPrefix(id, arnPrefix) || hasRegionalPrefix(id):
		return id, nil
	}
	return a.prefix + "." + id, nil
}

func hasRegionalPrefix(id string) bool {
	return slices.ContainsFunc(regionalPrefixes, func(prefix string) bool { return strings.HasPrefix(id, prefix) })
}

// modelSegment is a model id as one path segment: "/" (in ARNs) becomes
// "%2F" and ":" becomes "%3A", which url.PathEscape leaves alone but Bedrock
// expects encoded. The SigV4 canonical path escapes the segment once more.
func modelSegment(id string) string {
	return strings.ReplaceAll(url.PathEscape(id), ":", "%3A")
}

// invokePath is the InvokeModel path of a model id, or the
// InvokeModelWithResponseStream one.
func invokePath(id string, stream bool) string {
	action := "invoke"
	if stream {
		action = "invoke-with-response-stream"
	}
	return "/model/" + modelSegment(id) + "/" + action
}

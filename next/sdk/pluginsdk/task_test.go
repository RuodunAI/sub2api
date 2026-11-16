package pluginsdk

import (
	"context"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type taskTestPlatform struct{ Platform }

func (taskTestPlatform) ParseTaskSubmission(_ context.Context, in *pluginv1.ExtractUsageRequest) (*pluginv1.TaskSubmission, error) {
	return &pluginv1.TaskSubmission{UpstreamRefId: string(in.Body), SnapshotJson: `{"id":"job","status":"queued"}`}, nil
}

type ordinaryTestPlatform struct{ Platform }

func TestTaskSubmissionParserIsOptIn(t *testing.T) {
	ctx := context.Background()
	got, err := (platformServer{impl: taskTestPlatform{}}).ParseTaskSubmission(ctx, &pluginv1.ExtractUsageRequest{Body: []byte("job")})
	if err != nil || got.GetUpstreamRefId() != "job" {
		t.Fatalf("task parser failed: %v, %v", got, err)
	}
	_, err = (platformServer{impl: ordinaryTestPlatform{}}).ParseTaskSubmission(ctx, &pluginv1.ExtractUsageRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("ordinary platform must not silently register an empty task: %v", err)
	}
}

type rankOnly struct{ AccountRanker }

func TestCapabilitiesIncludesRankOnlyPlugin(t *testing.T) {
	caps := Capabilities(rankOnly{})
	if len(caps) != 1 || caps[0] != manifest.CapSchedulerRank {
		t.Fatalf("rank-only capability omitted: %v", caps)
	}
}

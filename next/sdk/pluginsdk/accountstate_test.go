package pluginsdk

import (
	"context"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"google.golang.org/grpc"
)

type automaticStateClient struct {
	pluginv1.HostServiceClient
	request *pluginv1.SetAccountAutomaticStateRequest
}

func (c *automaticStateClient) SetAccountAutomaticState(_ context.Context, in *pluginv1.SetAccountAutomaticStateRequest, _ ...grpc.CallOption) (*pluginv1.SetAccountAutomaticStateResponse, error) {
	c.request = in
	return &pluginv1.SetAccountAutomaticStateResponse{Changed: true, Status: "auto_disabled"}, nil
}

func TestSetAccountAutomaticStateRequiresHost(t *testing.T) {
	ctx := t.Context()
	if _, err := SetAccountAutomaticState(ctx, nil, nil); err != ErrHostNotReady {
		t.Fatal(err)
	}
	client := &automaticStateClient{}
	h := newHost(HostInfo{}, client, nil, false, 1)
	request := &pluginv1.SetAccountAutomaticStateRequest{AccountId: 23, State: &pluginv1.AccountAutomaticState{
		Action: pluginv1.AccountAutomaticState_ACTION_DISABLE, Reason: "quota", Release: &pluginv1.AccountAutomaticState_DurationSeconds{DurationSeconds: 60},
	}}
	out, err := SetAccountAutomaticState(ctx, h, request)
	if err != nil || !out.GetChanged() || client.request != request {
		t.Fatalf("host: %v %v", out, err)
	}
}

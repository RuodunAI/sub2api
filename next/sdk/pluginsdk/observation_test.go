package pluginsdk

import (
	"context"
	"testing"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/observation"
)

type testObservationPlugin struct{}

func (testObservationPlugin) OnObservations(context.Context, *pluginv1.ObservationBatch) (*pluginv1.ObservationResult, error) {
	return &pluginv1.ObservationResult{}, nil
}

func TestObservationCapability(t *testing.T) {
	rt, err := newRuntime(testObservationPlugin{}, buildOptions([]Option{WithInfo("probe", "1.0.0")}), nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := rt.GetInfo(context.Background(), &pluginv1.GetInfoRequest{})
	if err != nil || len(info.Capabilities) != 1 || info.Capabilities[0] != observation.Capability {
		t.Fatal(info, err)
	}
	_, err = ReadObservationMetrics(context.Background(), nil, &pluginv1.ReadObservationMetricsRequest{})
	if err != ErrHostNotReady {
		t.Fatal(err)
	}
}

package gateway

import (
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

func TestValidatePatchesComparesOnlyWhenBytesDiffer(t *testing.T) {
	newCall := func() *call {
		c := &call{ep: manifest.Endpoint{Protocol: "openai.chat", Billing: "free"}}
		c.ep.Request.ModelReferences = []manifest.RequestModelReference{{Name: "fallback", ArrayPath: "fallbacks", ModelPath: "model"}}
		return c
	}
	rt := &typeRoute{upstream: "openai.chat"}
	before := []byte(`{"fallbacks":[{"model":"a"}]}`)
	if kind, e := newCall().validatePatches(rt, before, before); kind != attemptDone || e != nil {
		t.Fatalf("self-comparison: kind %v err %v", kind, e)
	}
	after := []byte(`{"fallbacks":[{"model":"b"}]}`)
	if kind, e := newCall().validatePatches(rt, before, after); kind != attemptFailover || e == nil {
		t.Fatalf("changed references accepted: kind %v err %v", kind, e)
	}
}

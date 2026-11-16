package gateway

import (
	"context"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// A hook that replaced the body must invalidate the cached reference scan, or
// the second checkModel would authorize the old models.
func TestCheckModelRescansAfterSetBody(t *testing.T) {
	c := &call{
		gen: newFakeGen(), platform: "anthropic", rec: &core.UsageRecord{},
		principal: &core.APIKeyPrincipal{},
		body:      []byte(`{"model":"primary","fallbacks":[{"kind":"x","model":"first"}]}`),
	}
	c.ep.Protocol = "openai.chat"
	c.ep.Request.ModelPath = "model"
	c.ep.Request.ModelReferences = []manifest.RequestModelReference{{
		Name: "fallback", ArrayPath: "fallbacks", ModelPath: "model", Match: map[string]string{"kind": "x"},
	}}
	ctx := context.Background()
	if e := c.checkModel(ctx); e != nil {
		t.Fatal(e)
	}
	if len(c.modelRefs) != 1 || c.modelRefs[0].model != "first" {
		t.Fatalf("first scan: %+v", c.modelRefs)
	}
	c.setBody([]byte(`{"model":"primary","fallbacks":[{"kind":"x","model":"second"}]}`))
	if e := c.checkModel(ctx); e != nil {
		t.Fatal(e)
	}
	if len(c.modelRefs) != 1 || c.modelRefs[0].model != "second" {
		t.Fatalf("stale reference scan reused after setBody: %+v", c.modelRefs)
	}
}

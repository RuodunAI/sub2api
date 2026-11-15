package grpcruntime

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/plugin/registry"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

type auditConfigClient struct {
	pluginv1.PluginServiceClient
	calls atomic.Int32
}

func (c *auditConfigClient) Configure(context.Context, *pluginv1.ConfigureRequest, ...grpc.CallOption) (*pluginv1.ConfigureResponse, error) {
	if c.calls.Add(1) == 1 {
		return nil, fmt.Errorf("transient Configure unavailable")
	}
	return &pluginv1.ConfigureResponse{}, nil
}
func TestRefreshRetriesFailedConfigure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The plugin's settings as the runtime reads them: no config, no grants.
	db := testutil.DB(t)
	if _, err := db.SQL.ExecContext(ctx, `INSERT INTO plugins(key,name,status)VALUES('audit','{}','enabled')`); err != nil {
		t.Fatal(err)
	}
	rt := &Runtime{o: Options{DB: db}}
	i := &Instance{rt: rt, pkg: &registry.Package{Key: "audit"}, log: slog.Default()}
	i.settings.Store(&settings{configJSON: "{}", grants: registry.Grants{"lock": []byte("{}")}, fingerprint: "old-version"})
	client := &auditConfigClient{}
	i.proc.Store(&proc{plugin: client})
	if err := i.Refresh(ctx); err == nil {
		t.Fatal("first transient configure failure was expected")
	}
	if client.calls.Load() != 1 {
		t.Fatal("first Refresh failed before Configure")
	}
	if _, allowed := i.Grants()["lock"]; allowed {
		t.Fatal("revoked host permission survived failed Configure")
	}
	if err := i.Refresh(ctx); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if client.calls.Load() != 2 {
		t.Fatalf("Configure invoked %d times; second Refresh returned nil without retry after transient failure", client.calls.Load())
	}
}

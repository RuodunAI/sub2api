package pluginsdk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
)

type monitorTestPlatform struct {
	Platform
	TaskMonitor
}

type rankOnly struct{ AccountRanker }

func TestCapabilitiesAreInferredFromInterfaces(t *testing.T) {
	for name, tc := range map[string]struct {
		p    any
		want []string
	}{
		"platform":  {ordinaryTestPlatform{}, []string{manifest.CapPlatformAdapter}},
		"monitor":   {monitorTestPlatform{}, []string{manifest.CapPlatformAdapter, manifest.CapPlatformMonitor}},
		"rank only": {rankOnly{}, []string{manifest.CapSchedulerRank}},
	} {
		if got := Capabilities(tc.p); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func manifestWith(caps ...string) []byte {
	return []byte(`{"key":"p","version":"0.1.0","capabilities":[{"id":"` + strings.Join(caps, `"},{"id":"`) + `"}]}`)
}

// tasks.v1 and websocket.v1 are promises about Execute: reported when the
// manifest declares them, and only a Platform can make them.
func TestDeclaredPromisesAreReported(t *testing.T) {
	all := []string{manifest.CapPlatformAdapter, manifest.CapPlatformMonitor, manifest.CapPlatformTasks, manifest.CapPlatformWebSocket}
	rt, err := newRuntime(monitorTestPlatform{}, options{manifestRaw: manifestWith(all...)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rt.capabilities, all) {
		t.Fatalf("reported %v, want %v", rt.capabilities, all)
	}
	info, _ := rt.GetInfo(t.Context(), nil)
	if info.GetProtocolVersion() != 2 || protocol.Handshake.ProtocolVersion != 2 {
		t.Fatalf("plugin protocol %d, handshake %d, want 2", info.GetProtocolVersion(), protocol.Handshake.ProtocolVersion)
	}
	// Undeclared, an implemented capability is not reported.
	rt, err = newRuntime(monitorTestPlatform{}, options{manifestRaw: manifestWith(manifest.CapPlatformAdapter)}, nil)
	if err != nil || !slices.Equal(rt.capabilities, []string{manifest.CapPlatformAdapter}) {
		t.Fatalf("reported %v, %v", rt.capabilities, err)
	}
}

func TestManifestPromisesNeedTheirImplementation(t *testing.T) {
	for name, tc := range map[string]struct {
		p    any
		caps []string
	}{
		"websocket without a Platform":   {struct{}{}, []string{manifest.CapPlatformWebSocket}},
		"tasks without a Platform":       {rankOnly{}, []string{manifest.CapPlatformTasks}},
		"monitor without a TaskMonitor":  {ordinaryTestPlatform{}, []string{manifest.CapPlatformAdapter, manifest.CapPlatformMonitor}},
		"monitor without the adapter":    {monitorTestPlatform{}, []string{manifest.CapPlatformMonitor}},
		"tasks without a TaskMonitor":    {ordinaryTestPlatform{}, []string{manifest.CapPlatformAdapter, manifest.CapPlatformTasks}},
		"tasks without monitor declared": {monitorTestPlatform{}, []string{manifest.CapPlatformAdapter, manifest.CapPlatformTasks}},
	} {
		if _, err := newRuntime(tc.p, options{manifestRaw: manifestWith(tc.caps...)}, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A task endpoint is a promise even without platform.tasks.v1.
	raw := []byte(`{"key":"p","version":"0.1.0","capabilities":[{"id":"platform.adapter.v1"},{"id":"platform.monitor.v1"}],
		"platforms":[{"id":"x","endpoints":[{"protocol":"x.submit","task":{"action":"submit"}}]}]}`)
	if _, err := newRuntime(monitorTestPlatform{}, options{manifestRaw: raw}, nil); err == nil {
		t.Error("task endpoint without platform.tasks.v1 accepted")
	}
	// Inferred without a manifest, a monitor still needs its Platform.
	if _, err := newRuntime(struct{ TaskMonitor }{}, options{key: "p", version: "0.1.0"}, nil); err == nil {
		t.Error("TaskMonitor without a Platform accepted")
	}
}

// A platform plugin receives whole request bodies: its server takes gRPC's
// maximum without an option. Other plugins keep the default or their option.
func TestServerOptionsReceiveLimit(t *testing.T) {
	limit := func(p any, opts ...Option) int {
		t.Helper()
		sopts, err := ServerOptions(p, append(opts, WithInfo("p", "0.1.0"))...)
		if err != nil {
			t.Fatal(err)
		}
		if len(sopts) == 0 {
			return 0
		}
		return recvLimitOf(t, sopts)
	}
	if got := limit(ordinaryTestPlatform{}); got != maxMessageBytes {
		t.Errorf("platform: %d", got)
	}
	if got := limit(ordinaryTestPlatform{}, WithMaxRecvMsgSize(1<<20)); got != maxMessageBytes {
		t.Errorf("platform with a smaller option: %d", got)
	}
	if got := limit(rankOnly{}); got != 0 {
		t.Errorf("no platform: %d", got)
	}
	if got := limit(rankOnly{}, WithMaxRecvMsgSize(8<<20)); got != 8<<20 {
		t.Errorf("hook option: %d", got)
	}
	// Declared away, the plugin serves no platform and gets no raise.
	if got := limit(ordinaryTestPlatform{}, WithManifest(manifestWith(manifest.CapSchedulerRank))); got != 0 {
		t.Errorf("platform not declared: %d", got)
	}
	if _, err := ServerOptions(nil); err == nil {
		t.Error("nil plugin accepted")
	}
}

// recvLimitOf reads the receive limit a server built from sopts enforces.
func recvLimitOf(t *testing.T, sopts []grpc.ServerOption) int {
	t.Helper()
	s := grpc.NewServer(sopts...)
	defer s.Stop()
	v := reflect.ValueOf(s).Elem().FieldByName("opts").FieldByName("maxReceiveMessageSize")
	if !v.IsValid() {
		t.Fatal("grpc.Server no longer has opts.maxReceiveMessageSize")
	}
	return int(v.Int())
}

// The reporting callbacks and the old request path are gone: from the host
// contract, and from this package.
func TestReportingCallbacksAreGone(t *testing.T) {
	removed := []string{
		"RecordUsage", "ReserveAndWatch", "ReportTaskProgress", "ForwardUpstream",
		"ExecuteHTTP", "SetPatch", "DeletePatch", "Poll", "BuildUpstreamRequest", "BuildReconcileRequest", "ParseReconcileResponse",
	}
	removedTypes := []string{"Poller", "Reconciler"}
	host := reflect.TypeOf((*pluginv1.HostServiceClient)(nil)).Elem()
	platform := reflect.TypeOf((*pluginv1.PlatformServiceServer)(nil)).Elem()
	for _, name := range removed[:4] {
		if _, ok := host.MethodByName(name); ok {
			t.Errorf("HostService still has %s", name)
		}
	}
	for _, name := range []string{"Poll", "BuildUpstreamRequest", "ExtractUsage", "ParseTaskSubmission", "BuildReconcileRequest", "ParseReconcileResponse"} {
		if _, ok := platform.MethodByName(name); ok {
			t.Errorf("PlatformService still has %s", name)
		}
	}
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range pkgs["pluginsdk"].Files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if slices.Contains(removed, d.Name.Name) {
					t.Errorf("package pluginsdk still declares %s", d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && slices.Contains(removedTypes, ts.Name.Name) {
						t.Errorf("package pluginsdk still declares type %s", ts.Name.Name)
					}
				}
			}
		}
	}
}

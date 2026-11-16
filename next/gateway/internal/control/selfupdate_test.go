package control

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

func TestGatewayStepsSwapFollowersThenPrimary(t *testing.T) {
	p := Plan{Strategy: GatewayRolling, ID: "p", ReleaseDigest: "gw", SourceDigest: "core", Nodes: []string{"a", "b"}}
	var actions []string
	for i, s := range Steps(p) {
		actions = append(actions, s.NodeID+":"+s.Action+"@"+s.TargetDigest)
		if s.ID != i || s.PeerNode != "" {
			t.Fatalf("step %d: %+v", i, s)
		}
	}
	turn := func(n, swap string) []string {
		return []string{n + ":yield@gw", n + ":stop@gw", n + ":" + swap + "@gw", n + ":start@core", n + ":admit@core", n + ":local@core", n + ":gw-confirm@gw", n + ":resume@gw"}
	}
	want := append([]string{"a:gw-prepare@gw", "b:gw-prepare@gw"}, turn("b", "gw-swap")...)
	if want = append(want, turn("a", "gw-swap")...); !reflect.DeepEqual(actions, want) {
		t.Fatal(actions)
	}
	p.Strategy = GatewayRollback
	actions = nil
	for _, s := range Steps(p) {
		actions = append(actions, s.NodeID+":"+s.Action+"@"+s.TargetDigest)
	}
	if want = append(turn("b", "gw-revert"), turn("a", "gw-revert")...); !reflect.DeepEqual(actions, want) {
		t.Fatal(actions)
	}
	if p.CoreDigest() != "core" || (Plan{Strategy: Rolling, ReleaseDigest: "r", SourceDigest: "s"}).CoreDigest() != "r" {
		t.Fatal("core target")
	}
}

func TestGatewayNodes(t *testing.T) {
	now := time.Now()
	m := rc.GatewayManifest{GatewayVersion: "0.1.136"}
	node := func(id string) Node {
		return Node{ID: id, Enabled: true, Rolling: true, SelfUpdate: true, GatewayVersion: "0.1.135", ReleaseDigest: "base", Mode: "local", Ready: true, LastSeen: now}
	}
	if planned, got := gatewayNodes(m, []Node{node("a"), node("b")}, "base"); len(got) != 0 || !reflect.DeepEqual(planned, []string{"a", "b"}) {
		t.Fatal(planned, got)
	}
	cases := map[string]struct {
		code   string
		mutate func(*Node)
	}{
		"not locally ready":          {issueNodeNotReady, func(n *Node) { n.Ready = false }},
		"not on approved baseline":   {issueNodeOffBaseline, func(n *Node) { n.ReleaseDigest = "other" }},
		"without rolling updates":    {issueNodeNotRolling, func(n *Node) { n.Rolling = false }},
		"does not report":            {issueNodeUnsupported, func(n *Node) { n.GatewayVersion = "" }},
		"newer gateway":              {issueNodeUnsupported, func(n *Node) { n.GatewayVersion = "0.1.137" }},
		"cannot replace its gateway": {issueNodeUnsupported, func(n *Node) { n.SelfUpdate = false }},
	}
	for want, c := range cases {
		b := node("b")
		c.mutate(&b)
		_, got := gatewayNodes(m, []Node{node("a"), b}, "base")
		if len(got) != 1 || !strings.Contains(got[0].Message, want) || got[0].Code != c.code || got[0].Params["node"] != "b" {
			t.Errorf("%s: %+v", want, got)
		}
	}
	// A node on the target version is left out of the plan, whatever its state.
	current := node("b")
	current.GatewayVersion, current.Ready = "0.1.136", false
	if planned, got := gatewayNodes(m, []Node{node("a"), current}, "base"); len(got) != 0 || !reflect.DeepEqual(planned, []string{"a"}) {
		t.Fatalf("a current node: %v %v", planned, got)
	}
	if planned, got := gatewayNodes(m, []Node{current}, "base"); len(planned) != 0 || len(got) != 1 || got[0].Code != issueAlreadyCurrent {
		t.Fatalf("every node current: %v %v", planned, got)
	}
	disabled := node("b")
	disabled.Enabled, disabled.SelfUpdate = false, false
	if planned, got := gatewayNodes(m, []Node{node("a"), disabled}, "base"); len(got) != 0 || !reflect.DeepEqual(planned, []string{"a"}) {
		t.Fatalf("a disabled node: %v %v", planned, got)
	}
	if _, got := gatewayNodes(m, []Node{disabled}, "base"); len(got) != 1 || got[0].Code != issueNoNodes {
		t.Fatalf("no enabled node: %v", got)
	}
}

// installableGateway replaces the fixture's gateway archive by a real one: a
// script that reports the release version.
func (f *sourceFixture) installableGateway(t *testing.T) {
	t.Helper()
	bin := []byte("#!/bin/sh\necho " + f.gateway.GatewayVersion + "\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "sub2api-gateway", Mode: 0755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(bin)
	_ = tw.Close()
	_ = gz.Close()
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	f.gateway.Platforms = []rc.GatewayPlatform{{OS: "linux", Arch: "amd64", Archive: "real.tar.gz", ArchiveDigest: sum(buf.Bytes()), ArchiveBytes: int64(buf.Len()), BinaryDigest: sum(bin)}}
	f.publish()
	f.mu.Lock()
	f.files["gateway/real.tar.gz"] = buf.Bytes()
	f.mu.Unlock()
}

// gatewayCluster is a two-node cluster of rolling, self-updating gateways
// 0.1.135 (also their bundled version) whose update source offers gateway
// 0.1.136; a is the primary.
func gatewayCluster(t *testing.T) (*Store, *sourceFixture, []*Engine) {
	t.Helper()
	s, a, b, _ := setupEnginesWith(t, true)
	ctx := context.Background()
	f := newSourceFixture(t)
	f.installableGateway(t)
	s.Updates = f.service(t)
	if _, err := s.SetUpdateSource(ctx, SourceRequest{Enabled: true, Kind: "url", URL: fixtureURL}); err != nil {
		t.Fatal(err)
	}
	s.RevokePeer = func(context.Context, string) error { return nil }
	engines := []*Engine{a, b}
	for _, e := range engines {
		e.Runtime = &rollingRuntime{fakeRuntime: e.Runtime.(*fakeRuntime), store: s, node: e.Node.ID}
		e.Node.GatewayVersion, e.Node.SelfUpdate, e.BundledVersion = "0.1.135", true, "0.1.135"
		e.Gateway = &release.Manager{Root: t.TempDir(), TrustedKeys: f.keys, OS: "linux", Arch: "amd64"}
		if err := s.Register(ctx, e.Node); err != nil {
			t.Fatal(err)
		}
		if err := e.Heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return s, f, engines
}

// restart is the gateway process that follows e's handoff: a new boot of
// the version the node selects, on the same host (its runtime, whose core
// e stopped, and its <root>/gateway), starting with Recover.
func restart(t *testing.T, e *Engine) *Engine {
	t.Helper()
	ctx := context.Background()
	version := e.BundledVersion
	if p, err := e.Gateway.ReadGatewayPointer(); err == nil && !p.Bundled {
		version = p.Version
	}
	n := e.Node
	n.ShellBootID, n.GatewayVersion = n.ShellBootID+"+", version
	if err := e.Store.Register(ctx, n); err != nil {
		t.Fatal(err)
	}
	next := &Engine{Store: e.Store, Node: n, Runtime: e.Runtime, Gateway: e.Gateway, BundledVersion: e.BundledVersion}
	if err := next.Recover(ctx, false); err != nil {
		t.Fatalf("recover after the handoff: %v", err)
	}
	return next
}

// drive runs the plan until it leaves running or until stop: the primary
// (engines[0]) coordinates, every node works, and a node whose gateway hands
// off is replaced by its restarted process. It returns the handoffs.
func drive(t *testing.T, s *Store, engines []*Engine, id string, stop func(Plan) bool) (Plan, []string) {
	t.Helper()
	ctx := context.Background()
	var handoffs []string
	for i := 0; i < 400; i++ {
		_ = engines[0].Coordinate(ctx)
		for j, e := range engines {
			handed := false
			e.Handoff = func() { handed = true }
			_ = e.Work(ctx)
			if handed {
				// A single node has nobody to yield to (it was confirmed).
				if st := e.YieldState(); !st.Yielding && len(engines) > 1 {
					t.Fatalf("%s hands off without yielding its traffic", e.Node.ID)
				}
				handoffs = append(handoffs, e.Node.ID)
				engines[j] = restart(t, e)
			}
		}
		p, err := s.Plan(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != "running" || (stop != nil && stop(p)) {
			return p, handoffs
		}
	}
	t.Fatal("plan did not settle")
	return Plan{}, nil
}

func TestPostgresGatewayPlanSwapsEachNodeAndContinuesAfterTheHandoff(t *testing.T) {
	s, _, engines := gatewayCluster(t)
	ctx := context.Background()
	a, b := engines[0], engines[1]
	_, base, revision, _ := s.ClusterState(ctx)
	code, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "started" {
		t.Fatalf("apply: %d %v", code, out)
	}
	p, err := s.Plan(ctx, data["plan_id"].(string))
	if err != nil || p.Strategy != GatewayRolling || p.SourceDigest != base {
		t.Fatalf("plan: %+v %v", p, err)
	}
	r, err := s.GatewayRelease(ctx, p.ReleaseDigest)
	if err != nil || r.Manifest.GatewayVersion != "0.1.136" || r.SourceLocator != "url:"+fixtureURL || r.SourceAssets["real.tar.gz"].Size == 0 {
		t.Fatalf("imported gateway release: %+v %v", r, err)
	}
	if _, err = s.Release(ctx, p.ReleaseDigest); err == nil {
		t.Fatal("a gateway release was read as a core release")
	}
	rs, err := s.Releases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Digest == p.ReleaseDigest {
			t.Fatal("core releases list a gateway release")
		}
	}

	p, handoffs := drive(t, s, engines, p.ID, nil)
	if p.Status != "completed" {
		t.Fatalf("plan: %+v", p)
	}
	if !reflect.DeepEqual(handoffs, []string{b.Node.ID, a.Node.ID}) {
		t.Fatalf("handoffs %v: the follower first, then the primary", handoffs)
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.GatewayVersion != "0.1.136" || n.ReleaseDigest != base || !n.ServingLocal(time.Now()) || n.Offloading {
			t.Fatalf("node after the plan: %+v", n)
		}
	}
	for _, e := range engines {
		ptr, err := e.Gateway.ReadGatewayPointer()
		if err != nil || ptr.Version != "0.1.136" || ptr.Previous == nil || !ptr.Previous.Bundled || ptr.Previous.Version != "0.1.135" {
			t.Fatalf("%s pointer: %+v %v", e.Node.ID, ptr, err)
		}
		if st := e.YieldState(); st.Yielding || st.GatewayVersion != "0.1.136" || st.BundledVersion != "0.1.135" {
			t.Fatalf("%s: %+v", e.Node.ID, st)
		}
	}
	_, after, rev, _ := s.ClusterState(ctx)
	if after != base || rev <= revision {
		t.Fatalf("a gateway plan keeps the core baseline: %s %d", after, rev)
	}
	// Each restarted gateway's core came back on the baseline, without a
	// migration and without the rolling schema expectation.
	for _, e := range engines {
		opts := e.Runtime.(*rollingRuntime).options
		if last := opts[len(opts)-1]; last.AllowMigration || last.ExpectedSchemaBefore != "" {
			t.Fatalf("%s started its core with %+v", e.Node.ID, last)
		}
	}
	code, out = call(t, s, "GET", "/system/update-check", nil)
	if gw, _ := out["data"].(map[string]any)["gateway"].(map[string]any); code != 200 || gw["self_update"] != true {
		t.Fatalf("update check: %d %v", code, out)
	}
}

func TestPostgresGatewayRollbackReturnsSwappedNodesToTheirPreviousGateway(t *testing.T) {
	s, _, engines := gatewayCluster(t)
	ctx := context.Background()
	b := engines[1]
	_, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	id := out["data"].(map[string]any)["plan_id"].(string)
	// Stop once the follower runs the new gateway and its core serves again.
	p, handoffs := drive(t, s, engines, id, func(p Plan) bool {
		for _, st := range p.Steps {
			if st.NodeID == b.Node.ID && st.Action == "gw-confirm" && st.Status == "done" {
				return true
			}
		}
		return false
	})
	if p.Status != "running" || len(handoffs) != 1 || engines[1].Node.GatewayVersion != "0.1.136" {
		t.Fatalf("before rollback: %+v %v", p, handoffs)
	}
	if _, err := s.Action(ctx, id, "rollback", "admin"); err == nil {
		t.Fatal("a running plan was rolled back")
	}
	if _, err := s.Action(ctx, id, "pause", "admin"); err != nil {
		t.Fatal(err)
	}
	recovery, err := s.Action(ctx, id, "rollback", "admin")
	if err != nil || recovery.Strategy != GatewayRollback || recovery.ReleaseDigest != p.ReleaseDigest {
		t.Fatalf("rollback: %+v %v", recovery, err)
	}
	recovery, handoffs = drive(t, s, engines, recovery.ID, nil)
	if recovery.Status != "completed" {
		t.Fatalf("recovery: %+v", recovery)
	}
	if !reflect.DeepEqual(handoffs, []string{b.Node.ID}) {
		t.Fatalf("only the swapped follower hands off: %v", handoffs)
	}
	for _, e := range engines {
		if e.Node.GatewayVersion != "0.1.135" {
			t.Fatalf("%s runs %s", e.Node.ID, e.Node.GatewayVersion)
		}
	}
	if ptr, err := engines[1].Gateway.ReadGatewayPointer(); err != nil || !ptr.Bundled || ptr.Version != "0.1.135" {
		t.Fatalf("follower pointer: %+v %v", ptr, err)
	}
	if _, err := engines[0].Gateway.ReadGatewayPointer(); err == nil {
		t.Fatal("the primary never switched, so it records nothing")
	}
	if _, err := s.Action(ctx, recovery.ID, "rollback", "admin"); err == nil {
		t.Fatal("a completed recovery was rolled back")
	}
	events, err := s.Events(ctx, id, 0)
	if err != nil || !strings.Contains(stringify(events), "replaced by gateway rollback "+recovery.ID) || strings.Contains(stringify(events), "baseline recovery") {
		t.Fatalf("events of the rolled back plan: %v %v", events, err)
	}
}

// TestPostgresGatewayPlanLeavesOutCurrentNodes: a primary that already runs
// the target gateway is no member of the plan, yet coordinates it.
func TestPostgresGatewayPlanLeavesOutCurrentNodes(t *testing.T) {
	s, _, engines := gatewayCluster(t)
	ctx := context.Background()
	a, b := engines[0], engines[1]
	a.Node.GatewayVersion = "0.1.136"
	if err := s.nodeRow(s.db(ctx), a.Node.ID).Update("gateway_version", a.Node.GatewayVersion).Error; err != nil {
		t.Fatal(err)
	}
	code, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "started" {
		t.Fatalf("apply: %d %v", code, out)
	}
	p, err := s.Plan(ctx, data["plan_id"].(string))
	if err != nil || !reflect.DeepEqual(p.Nodes, []string{b.Node.ID}) {
		t.Fatalf("plan members: %+v %v", p, err)
	}
	primary := a.Runtime.(*rollingRuntime)
	calls := len(primary.calls)
	p, handoffs := drive(t, s, engines, p.ID, nil)
	if p.Status != "completed" || !reflect.DeepEqual(handoffs, []string{b.Node.ID}) || engines[1].Node.GatewayVersion != "0.1.136" {
		t.Fatalf("plan: %+v %v", p, handoffs)
	}
	// Outside the plan, the primary kept serving its core and took b's traffic.
	if len(primary.calls) != calls || slices.ContainsFunc(primary.log, func(e string) bool { return e != "serve-local" }) {
		t.Fatalf("the primary outside the plan: calls %v, log %v", primary.calls[calls:], primary.log)
	}
	// Nothing left to update.
	code, out = call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	if c, _ := errorCode(out); code != 409 || c != "preflight_blocked" || !strings.Contains(stringify(out), issueAlreadyCurrent) {
		t.Fatalf("all current: %d %v", code, out)
	}
}

func TestPostgresApplyGatewayConfirmsASingleNodeAndBlocks(t *testing.T) {
	s, _, engines := gatewayCluster(t)
	ctx := context.Background()
	// Blocked: the follower cannot replace its binary.
	b := engines[1].Node
	b.SelfUpdate = false
	if err := s.Register(ctx, b); err != nil {
		t.Fatal(err)
	}
	code, out := call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	if c, _ := errorCode(out); code != 409 || c != "preflight_blocked" || !strings.Contains(stringify(out), "cannot replace its gateway binary") {
		t.Fatalf("blocked: %d %v", code, out)
	}
	codes := out["error"].(map[string]any)["details"].(map[string]any)["blocker_codes"]
	if want := []any{map[string]any{"code": issueNodeUnsupported, "params": map[string]any{"node": b.ID},
		"message": "node " + b.ID + " cannot replace its gateway binary (it is not Linux, or <root>/gateway is not writable)"}}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("blocker codes: %v", codes)
	}
	code, out = call(t, s, "GET", "/system/update-check", nil)
	if gw, _ := out["data"].(map[string]any)["gateway"].(map[string]any); code != 200 || gw["self_update"] != false {
		t.Fatalf("update check: %d %v", code, out)
	}
	// A single node has nobody to hand its traffic to.
	if err := s.DisableNode(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	code, out = call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136"})
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["status"] != "needs_confirmation" || !strings.Contains(stringify(data["reasons"]), "single node") {
		t.Fatalf("unconfirmed: %d %v", code, out)
	}
	if codes := stringify(data["reason_codes"]); codes != `[{"code":"single_node","message":"single node: about 10 s interruption","params":{"seconds":"10"}}]` {
		t.Fatalf("reason codes: %s", codes)
	}
	if plans, err := s.Plans(ctx); err != nil || len(plans) != 0 {
		t.Fatalf("plan created without confirmation: %d %v", len(plans), err)
	}
	code, out = call(t, s, "POST", "/system/updates/apply", map[string]any{"line": "gateway", "version": "0.1.136", "confirm": true})
	if data, _ = out["data"].(map[string]any); code != 200 || data["status"] != "started" {
		t.Fatalf("confirmed: %d %v", code, out)
	}
	p, err := s.Plan(ctx, data["plan_id"].(string))
	if err != nil || !reflect.DeepEqual(p.Nodes, []string{engines[0].Node.ID}) {
		t.Fatalf("single-node plan: %+v %v", p, err)
	}
	p, handoffs := drive(t, s, engines[:1], p.ID, nil)
	if p.Status != "completed" || len(handoffs) != 1 || engines[0].Node.GatewayVersion != "0.1.136" {
		t.Fatalf("single node: %+v %v", p, handoffs)
	}
}

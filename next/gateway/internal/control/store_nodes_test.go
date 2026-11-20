package control

import (
	"context"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// remove-node, set-primary and the plugin package sources used tables that
// the gateway schema never created (updater.node_artifacts,
// updater.node_plugin_packages); each failed with "relation does not exist".
// Package sources are the other enabled nodes, whatever the primary is.
func TestPostgresRemoveNodeSetPrimaryAndPackageFallback(t *testing.T) {
	s, a, b, _ := setupEngines(t)
	ctx := context.Background()
	s.RevokePeer = func(context.Context, string) error { return nil }

	for self, other := range map[string]string{a.Node.ID: b.Node.ID, b.Node.ID: a.Node.ID} {
		peers, err := packagePeers(ctx, s, self)
		if err != nil || len(peers) != 1 || peers[0].ID != other || peers[0].PeerURL == "" {
			t.Fatalf("package sources of %s: %v %v", self, peers, err)
		}
	}

	if err := s.SetPrimary(ctx, b.Node.ID); err != nil {
		t.Fatalf("set-primary: %v", err)
	}
	if primary, _, _, _ := s.ClusterState(ctx); primary != b.Node.ID {
		t.Fatalf("primary = %s", primary)
	}
	if err := s.SetPrimary(ctx, a.Node.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveNode(ctx, b.Node.ID); err == nil || !strings.Contains(err.Error(), "still enabled") {
		t.Fatalf("enabled node removed: %v", err)
	}
	if err := s.DisableNode(ctx, b.Node.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db(ctx).Exec(`UPDATE updater.nodes SET last_seen=now()-interval '10 minutes' WHERE cluster_id=? AND node_id=?`, s.Cluster, b.Node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrimary(ctx, b.Node.ID); err == nil {
		t.Fatal("disabled node became primary")
	}
	if peers, _ := packagePeers(ctx, s, a.Node.ID); len(peers) != 0 {
		t.Fatalf("disabled node offered as package source: %v", peers)
	}
	if err := s.RemoveNode(ctx, b.Node.ID); err != nil {
		t.Fatalf("remove-node: %v", err)
	}
	var left int
	if err := s.db(ctx).Raw(`SELECT (SELECT count(*) FROM updater.nodes WHERE cluster_id=? AND node_id=?) + (SELECT count(*) FROM updater.node_admissions WHERE cluster_id=? AND node_id=?)`, s.Cluster, b.Node.ID, s.Cluster, b.Node.ID).Row().Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left %d %v", left, err)
	}
	if err := s.RemoveNode(ctx, a.Node.ID); err == nil {
		t.Fatal("enabled primary removed")
	}
}

// A node disabled by hand (the second node of a cluster cut down to one) is
// left out of a core plan instead of joining it and pausing it at its first
// step ("planned node is disabled").
func TestPostgresPreflightLeavesDisabledNodesOut(t *testing.T) {
	s, a, b, target := setupEngines(t)
	ctx := context.Background()
	s.RevokePeer = func(context.Context, string) error { return nil }
	if err := s.DisableNode(ctx, b.Node.ID); err != nil {
		t.Fatal(err)
	}
	pf, err := s.Preflight(ctx, target.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(pf.Nodes) != 1 || pf.Nodes[0] != a.Node.ID || len(pf.Blockers) != 0 {
		t.Fatalf("preflight nodes %v, blockers %v", pf.Nodes, pf.Blockers)
	}
}

// A stop-and-redeploy (CONTRACTS §34) waits for every gateway to stop and for
// no plan to be in flight; then the primary's serve -bootstrap starts the new
// release as a first start (migrations allowed) and a follower starts it as
// it is, no plan involved.
func TestPostgresRedeployStartsTheReleaseAsAFirstStart(t *testing.T) {
	s, a, b, target := setupEngines(t)
	ctx := context.Background()
	// The cluster first runs the old baseline: both nodes admitted it.
	if err := a.Recover(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Recover(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{a, b} {
		if err := e.Heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Redeploy(ctx, target.Digest); err == nil || !strings.Contains(err.Error(), "stop every gateway") {
		t.Fatalf("redeploy with live nodes: %v", err)
	}
	// The gateways stop: their telemetry expires.
	for _, e := range []*Engine{a, b} {
		if err := s.Redis.Del(ctx, s.telemetryKey(e.Node)).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// A core registered as live (draining after its gateway's heartbeat
	// stopped) blocks the redeploy until its registration goes.
	if err := s.Redis.ZAdd(ctx, "node:live", redis.Z{Score: 1, Member: "core-a"}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Redis.HSet(ctx, "node:info:core-a", "node_id", a.Node.ID).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(ctx, target.Digest); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("redeploy with a live core: %v", err)
	}
	if err := s.Redis.Del(ctx, "node:info:core-a").Err(); err != nil {
		t.Fatal(err)
	}
	// So does a plan in flight; it has to be finished or cancelled first.
	if err := s.createUpgrade(s.db(ctx), "plan-in-flight", target.Digest, []byte(`[]`), "test", "redeploy-test", "", Rolling); err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(ctx, target.Digest); err == nil || !strings.Contains(err.Error(), "running or paused") {
		t.Fatalf("redeploy with a plan in flight: %v", err)
	}
	if err := s.db(ctx).Table("updater.upgrades").Where("id = ?", "plan-in-flight").Update("status", "cancelled").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Redeploy(ctx, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("redeploy to a release never imported")
	}
	if err := s.Redeploy(ctx, target.Digest); err != nil {
		t.Fatal(err)
	}
	if _, baseline, _, _ := s.ClusterState(ctx); baseline != target.Digest {
		t.Fatalf("baseline %s", baseline)
	}
	primary, follower := a.Runtime.(*fakeRuntime), b.Runtime.(*fakeRuntime)
	primary.options, follower.options = nil, nil
	if err := a.Recover(ctx, true); err != nil {
		t.Fatal("primary", err)
	}
	if err := b.Recover(ctx, false); err != nil {
		t.Fatal("follower", err)
	}
	if len(primary.options) != 1 || !primary.options[0].Bootstrap || !primary.options[0].AllowMigration {
		t.Fatalf("primary start %+v", primary.options)
	}
	if len(follower.options) != 1 || follower.options[0].AllowMigration || follower.status.ReleaseDigest != target.Digest {
		t.Fatalf("follower start %+v on %s", follower.options, follower.status.ReleaseDigest)
	}
}

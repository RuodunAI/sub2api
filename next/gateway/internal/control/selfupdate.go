package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/Masterminds/semver/v3"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

// One-click gateway updates (CONTRACTS §102.9): a
// GatewayRolling plan installs a gateway release on every node and
// then, one node at a time, swaps the node's gateway process for it while
// its baseline core restarts; GatewayRollback swaps back.

var errNoSelfUpdate = errors.New("this node cannot replace its gateway binary")

// gatewayPreflight checks a gateway plan for the gateway release digest. The
// plan leaves out the nodes that already run its version, the primary
// included: it still coordinates.
func (s *Store) gatewayPreflight(ctx context.Context, digest string) (Preflight, error) {
	p := Preflight{ReleaseDigest: digest, Nodes: []string{}, Blockers: []string{}, BlockerCodes: []Issue{}, Strategy: GatewayRolling}
	primary, base, rev, err := s.ClusterState(ctx)
	if err != nil {
		return p, err
	}
	p.PrimaryNode, p.ExpectedRevision = primary, rev
	target, err := s.GatewayRelease(ctx, digest)
	if err != nil {
		return p, err
	}
	current, err := s.Release(ctx, base)
	if err != nil {
		return p, err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return p, err
	}
	p.block(issues(issueReleaseIncompatible, gatewayCompatibility(target.Manifest, current, nodes))...)
	planned, blockers := gatewayNodes(target.Manifest, nodes, base)
	p.Nodes = append(p.Nodes, planned...)
	p.block(blockers...)
	if err = s.blockBusy(ctx, &p); err != nil {
		return p, err
	}
	orderPrimaryFirst(p.Nodes, primary)
	if !slices.ContainsFunc(nodes, func(n Node) bool { return n.ID == primary && n.Enabled }) {
		p.block(primaryUnregistered)
	}
	return s.activeBlocker(ctx, p)
}

// gatewayNodes are the enabled nodes a plan switches to m, those not on its
// version yet, and the reasons they cannot: each must serve the baseline
// core, report an older gateway version and be able to install and exec a
// gateway binary. A disabled node keeps its gateway; it could not run its
// steps.
func gatewayNodes(m rc.GatewayManifest, nodes []Node, baseline string) ([]string, []Issue) {
	target, err := semver.StrictNewVersion(m.GatewayVersion)
	if err != nil {
		return nil, []Issue{{Code: issueReleaseIncompatible, Message: "the release has no valid gateway version"}}
	}
	planned, out := []string{}, []Issue{}
	for _, n := range nodes {
		v, err := semver.NewVersion(n.GatewayVersion)
		if !n.Enabled || (err == nil && v.Equal(target)) {
			continue
		}
		planned = append(planned, n.ID)
		if !n.ServingLocal(time.Now()) {
			out = append(out, nodeIssue(issueNodeNotReady, n.ID, "node not locally ready: "+n.ID))
		}
		if n.ReleaseDigest != baseline {
			out = append(out, nodeIssue(issueNodeOffBaseline, n.ID, "node not on approved baseline: "+n.ID))
		}
		if err != nil {
			out = append(out, nodeIssue(issueNodeUnsupported, n.ID, "node "+n.ID+" does not report its gateway version"))
		} else if v.GreaterThan(target) {
			out = append(out, nodeIssue(issueNodeUnsupported, n.ID, fmt.Sprintf("node %s already runs the newer gateway %s", n.ID, n.GatewayVersion)))
		}
		if !n.SelfUpdate {
			out = append(out, nodeIssue(issueNodeUnsupported, n.ID, "node "+n.ID+" cannot replace its gateway binary (it is not Linux, or <root>/gateway is not writable)"))
		}
	}
	switch {
	case enabledNodes(nodes) == 0:
		out = append(out, Issue{Code: issueNoNodes, Message: "upgrade requires at least one registered node"})
	case len(planned) == 0:
		out = append(out, Issue{Code: issueAlreadyCurrent, Message: "every enabled node already runs gateway " + m.GatewayVersion})
	}
	return planned, out
}

// yieldStep hands this node's new requests to the other serving nodes
// before its core restarts. A core that is not serving has nothing to hand
// over; one that is must have a taker, or the plan pauses here rather than
// go dark. The gateway plan of a single-node cluster, confirmed as an
// interruption, has no taker. A gateway plan also drains readiness, so that
// an ingress with a health check moves on before the gateway process
// restarts.
func (e *Engine) yieldStep(ctx context.Context, st Step) error {
	p, err := e.Store.Plan(ctx, st.UpgradeID)
	if err != nil {
		return err
	}
	gateway := gatewayStrategy(p.Strategy)
	if err = e.Yield(ctx, gateway); err == nil || (gateway && e.Store.singleNode(ctx)) {
		return nil
	}
	if stopped, _ := e.Runtime.Stopped(ctx); stopped {
		e.yielding.Store(true)
		return nil
	}
	if status, _, statusErr := e.Runtime.Status(ctx); statusErr != nil || !status.Ready {
		e.yielding.Store(true)
		return nil
	}
	return err
}

// singleNode reports a cluster of one enabled node.
func (s *Store) singleNode(ctx context.Context) bool {
	nodes, err := s.Nodes(ctx)
	return err == nil && enabledNodes(nodes) == 1
}

// prepareGateway installs the gateway release digest from the update
// source (release.Manager.InstallGateway verifies it end to end).
func (e *Engine) prepareGateway(ctx context.Context, digest string) error {
	if e.Gateway == nil {
		return errNoSelfUpdate
	}
	r, err := e.Store.GatewayRelease(ctx, digest)
	if err != nil {
		return err
	}
	installed, _, err := e.Gateway.InstallGateway(ctx, r.Raw, func(ctx context.Context, archive string, size int64) (io.ReadCloser, error) {
		return e.Store.openAsset(ctx, r.SourceAssets, r.SourceLocator, archive, size)
	})
	if err == nil && installed != digest {
		err = errors.New("stored gateway manifest digest mismatch")
	}
	return err
}

// swapGateway points this node's gateway at the step's release (gw-swap)
// or back at what ran before it (gw-revert). The core stops first: it must
// not outlive the process that supervises it. When the running binary is
// not the one the node now selects, the process restarts once the step is
// recorded (Work, Handoff), and the new one continues the plan.
func (e *Engine) swapGateway(ctx context.Context, st Step) error {
	if e.Gateway == nil {
		return errNoSelfUpdate
	}
	r, err := e.Store.GatewayRelease(ctx, st.TargetDigest)
	if err != nil {
		return err
	}
	if err = e.stop(ctx, fmt.Sprintf("%s-%d", st.UpgradeID, st.ID)); err != nil {
		return err
	}
	if st.Action == "gw-revert" {
		err = e.Gateway.RevertGateway(r.Manifest.GatewayVersion)
	} else {
		var p rc.GatewayPlatform
		if p, err = e.Gateway.GatewayPlatform(r.Manifest); err == nil {
			err = e.Gateway.SwitchGateway(r.Manifest.GatewayVersion, p.BinaryDigest, e.Node.GatewayVersion)
		}
	}
	if err != nil {
		return err
	}
	want, err := e.selectedGateway()
	e.handoff.Store(err == nil && want != e.Node.GatewayVersion)
	return err
}

// selectedGateway is the version this node's gateway starts: the installed
// one <root>/gateway/current names, or the bundled one.
func (e *Engine) selectedGateway() (string, error) {
	p, err := e.Gateway.ReadGatewayPointer()
	switch {
	case errors.Is(err, os.ErrNotExist) || (err == nil && p.Bundled):
		if e.BundledVersion != "" {
			return e.BundledVersion, nil
		}
		return e.Node.GatewayVersion, nil
	case err != nil:
		return "", err
	}
	return p.Version, nil
}

// confirmGateway checks that this node runs, and registered, the gateway it
// selects, then removes the installed versions a rollback cannot need.
func (e *Engine) confirmGateway(ctx context.Context) error {
	if e.Gateway == nil {
		return errNoSelfUpdate
	}
	want, err := e.selectedGateway()
	if err != nil {
		return err
	}
	var registered string
	if err = e.Store.nodeRow(e.Store.db(ctx), e.Node.ID).Where("shell_boot_id = ?", e.Node.ShellBootID).Select("gateway_version").Row().Scan(&registered); err != nil {
		return err
	}
	if e.Node.GatewayVersion != want || registered != want {
		return fmt.Errorf("node runs gateway %s (registered %q), expected %s", e.Node.GatewayVersion, registered, want)
	}
	if err = e.Gateway.PruneGateways(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("old gateway versions were not removed", "node", e.Node.ID, "err", err)
	}
	return nil
}

// Package control implements the shell's durable, serial rolling-update controller.
package control

import (
	"context"
	"slices"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/updatesource"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
)

type Node struct {
	TelemetryBootID string    `json:"-"`
	Enabled         bool      `json:"enabled"`
	PeerProtocol    int       `json:"peer_protocol"`
	Strategy        string    `json:"strategy"`
	JoiningPlan     string    `json:"joining_plan,omitempty"`
	Stopped         bool      `json:"stopped"`
	ID              string    `json:"node_id"`
	PeerURL         string    `json:"peer_url"`
	ShellBootID     string    `json:"shell_boot_id"`
	ReleaseDigest   string    `json:"release_digest"`
	CoreBootID      string    `json:"core_boot_id"`
	Mode            string    `json:"mode"`
	Ready           bool      `json:"ready"`
	LastSeen        time.Time `json:"last_seen"`
	Error           string    `json:"error,omitempty"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch"`
	RuntimeABI      string    `json:"runtime_abi"`
	RouteRevision   int64     `json:"route_revision"`
	// GatewayVersion is the version of the node's gateway binary, written at
	// registration; empty for a gateway that predates it.
	GatewayVersion string `json:"gateway_version"`
	// CPUPercent is the node's averaged CPU load; nil when it is not measured.
	CPUPercent *float64 `json:"cpu_percent"`
	// Offloading is set before the node sends its new requests elsewhere and
	// cleared after it stopped; receivers authorize offloaded forwards by it.
	Offloading bool `json:"offloading"`
	// Rolling marks a shell that executes rolling-v1 plans: it can yield its
	// traffic to any serving node while its own core restarts. Older shells
	// leave it false, and a cluster with one of them upgrades primary-first.
	Rolling bool `json:"rolling"`
	// SelfUpdate marks a Linux gateway that can install gateway binaries
	// under <root>/gateway and exec them: a gateway plan needs it everywhere.
	SelfUpdate bool `json:"self_update"`
}

// nodeFreshness is the age beyond which a heartbeat observation is stale. Every
// routing and admission decision that needs a live node reads it through
// Node.Fresh/ServingLocal so the window cannot drift between call sites.
const nodeFreshness = 20 * time.Second

// Fresh reports whether the node's last heartbeat observation is recent enough
// to be trusted. It deliberately ignores Readiness, Mode and Enabled: callers
// that only need liveness (for example follower observation checks) add those
// conditions themselves.
func (n Node) Fresh(now time.Time) bool {
	return !n.LastSeen.IsZero() && now.Sub(n.LastSeen) < nodeFreshness
}

// ServingLocal reports whether the node is fresh, ready and serving its own core
// locally. It deliberately ignores Enabled: several callers route to a node
// regardless of scheduling state, and folding Enabled in would change where
// requests for disabled nodes are forwarded.
func (n Node) ServingLocal(now time.Time) bool {
	return n.Fresh(now) && n.Ready && n.Mode == "local"
}

type Release struct {
	Digest     string            `json:"digest"`
	Manifest   rc.Manifest       `json:"manifest"`
	Signed     rc.SignedManifest `json:"-"`
	BundleBase string            `json:"-"`
	// SourceAssets are the assets, by file name, of a release imported from
	// the update source at SourceLocator (sourceRow.locator); its bundles are
	// downloaded through that source and BundleBase is empty.
	SourceAssets  map[string]updatesource.Asset `json:"-"`
	SourceLocator string                        `json:"-"`
}

// GatewayRelease is a signed gateway release imported from the update
// source; its archives are downloaded through that source.
type GatewayRelease struct {
	Digest        string                        `json:"digest"`
	Manifest      rc.GatewayManifest            `json:"manifest"`
	Signed        rc.SignedManifest             `json:"-"`
	SourceAssets  map[string]updatesource.Asset `json:"-"`
	SourceLocator string                        `json:"-"`
}
type Plan struct {
	Strategy      string    `json:"strategy"`
	SourceDigest  string    `json:"source_digest"`
	ID            string    `json:"id"`
	ReleaseDigest string    `json:"release_digest"`
	Nodes         []string  `json:"nodes"`
	Status        string    `json:"status"`
	Cursor        int       `json:"cursor"`
	Error         string    `json:"error,omitempty"`
	Actor         string    `json:"actor"`
	CreatedAt     time.Time `json:"created_at"`
	Steps         []Step    `json:"steps"`
}
type Step struct {
	UpgradeID    string `json:"upgrade_id"`
	ID           int    `json:"step_id"`
	NodeID       string `json:"node_id"`
	Action       string `json:"action"`
	TargetDigest string `json:"target_digest"`
	PeerNode     string `json:"peer_node,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}
type Preflight struct {
	ReleaseDigest    string   `json:"release_digest"`
	ExpectedRevision int64    `json:"expected_revision"`
	PrimaryNode      string   `json:"primary_node"`
	Nodes            []string `json:"nodes"`
	Blockers         []string `json:"blockers"`
	// BlockerCodes are the Blockers as Issues, in the same order.
	BlockerCodes []Issue `json:"blocker_codes"`
	Strategy     string  `json:"strategy"`
}

// block adds blockers to p.
func (p *Preflight) block(blockers ...Issue) {
	for _, b := range blockers {
		p.Blockers = append(p.Blockers, b.Message)
		p.BlockerCodes = append(p.BlockerCodes, b)
	}
}

// Issue is a preflight blocker or an apply confirmation reason: a stable
// code with its parameters, for the console to word, and the English
// message. The codes, and their params:
//
//	release_incompatible  the signed release cannot follow the installed one
//	plugin_incompatible   an active plugin does not support the target core
//	plugins_changing      a plugin install, upgrade or removal is in progress
//	core_unmanaged        a core the gateway does not manage is serving
//	no_nodes              no enabled node is registered
//	already_current       every enabled node already runs the target gateway
//	primary_unregistered  the upgrade primary is not a registered node
//	upgrade_active        another plan is active
//	node_not_ready        {node} is not serving its own core
//	node_off_baseline     {node} does not run the approved baseline
//	node_unsupported      {node} cannot run this plan: an old shell, its
//	                      platform, its gateway version, or no self-update
//	node_not_rolling      {node} runs a gateway without rolling updates
//	maintenance_release   the release is signed for a maintenance update
//	schema_change         the release changes the database schema
//	too_few_serving       {min} fewer nodes serve than a rolling update needs
//	single_node           {seconds} the only node restarts its gateway with
//	                      nobody to take its traffic
type Issue struct {
	Code    string            `json:"code"`
	Params  map[string]string `json:"params,omitempty"`
	Message string            `json:"message"`
}

const (
	issueReleaseIncompatible = "release_incompatible"
	issuePluginIncompatible  = "plugin_incompatible"
	issuePluginsChanging     = "plugins_changing"
	issueCoreUnmanaged       = "core_unmanaged"
	issueNoNodes             = "no_nodes"
	issueAlreadyCurrent      = "already_current"
	issuePrimaryUnregistered = "primary_unregistered"
	issueUpgradeActive       = "upgrade_active"
	issueNodeNotReady        = "node_not_ready"
	issueNodeOffBaseline     = "node_off_baseline"
	issueNodeUnsupported     = "node_unsupported"
	issueNodeNotRolling      = "node_not_rolling"
	issueMaintenanceRelease  = "maintenance_release"
	issueSchemaChange        = "schema_change"
	issueTooFewServing       = "too_few_serving"
	issueSingleNode          = "single_node"
)

// nodeIssue is an issue of one node.
func nodeIssue(code, node, message string) Issue {
	return Issue{Code: code, Params: map[string]string{"node": node}, Message: message}
}

// issues gives every text the same code.
func issues(code string, texts []string) []Issue {
	out := make([]Issue, 0, len(texts))
	for _, m := range texts {
		out = append(out, Issue{Code: code, Message: m})
	}
	return out
}

// messages are the English messages of is.
func messages(is []Issue) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Message)
	}
	return out
}

type Event struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// Runtime owns all local process and routing operations. Implementations must be
// restart-idempotent and stop promptly when context is cancelled.
type Runtime interface {
	Prepare(context.Context, Release, string) error
	Redirect(context.Context, Node) error
	DrainStop(context.Context, string) error
	Start(context.Context, string, rc.PrepareRequest) (rc.Status, error)
	Maintenance(context.Context) error
	Stopped(context.Context) (bool, error)
	Admit(context.Context, rc.Admission) (rc.Status, error)
	Local(context.Context) error
	Status(context.Context) (rc.Status, string, error)
	RouteRevision() int64
}
type LockRunner interface {
	WithLock(context.Context, string, time.Duration, func(context.Context) error) (bool, error)
}

// PrimaryFirst stops every follower, migrates and restarts the primary, then
// restarts the followers: the only strategy for a release that changes the
// schema, and the fallback whenever a node cannot run Rolling.
const PrimaryFirst = "primary-first-v1"

// Rolling (2026-11-08) restarts one node at a time while the others keep
// serving. Each node first yields its new requests to the serving nodes
// (Node.Offloading, the CPU offload channel), drains and restarts its core
// on the target release, serves locally again and takes its traffic back.
// Only a release whose SchemaBefore equals its SchemaAfter and whose signed
// strategy is "rolling" qualifies, and every enabled node must be a Rolling
// shell with at least two of them serving (Preflight).
const Rolling = "rolling-v1"

// GatewayRolling (2026-11-17) replaces the gateway binary of one node at a
// time, followers first, while the others serve: each node installs the
// signed release (gw-prepare), yields, stops its core, points
// <root>/gateway/current at the new binary and execs it (gw-swap), starts
// and admits the unchanged baseline core again, checks that the new gateway
// registered (gw-confirm) and takes its traffic back. GatewayRollback undoes
// the swaps of a paused GatewayRolling plan the same way (gw-revert). Both
// keep the core release: their cores run the plan's source digest, the
// cluster baseline.
const (
	GatewayRolling  = "gateway-rolling-v1"
	GatewayRollback = "gateway-rollback-v1"
)

// gatewayStrategy reports a plan that replaces gateway binaries.
func gatewayStrategy(s string) bool { return s == GatewayRolling || s == GatewayRollback }

// rollingStrategy reports a plan that restarts one node at a time.
func rollingStrategy(s string) bool { return s == Rolling || gatewayStrategy(s) }

// coreTarget is the core release the nodes of a plan of strategy run: its
// release, or for a gateway plan the baseline it started from (source).
func coreTarget(strategy, release, source string) string {
	if gatewayStrategy(strategy) {
		return source
	}
	return release
}

// leaves reports a node outside gateway plan p. A gateway plan keeps the
// core baseline, so such a node serves as if no plan ran.
func (p Plan) leaves(node string) bool {
	return gatewayStrategy(p.Strategy) && !slices.Contains(p.Nodes, node)
}

// CoreDigest is the core release p's nodes run once it completes.
func (p Plan) CoreDigest() string { return coreTarget(p.Strategy, p.ReleaseDigest, p.SourceDigest) }

// PeerProtocol 2: Redis registrations carry the SHA-256 of the node key
// (peer.Protocol); every gateway of a cluster must agree.
const PeerProtocol = 2

// Steps are the steps of p under its persisted strategy.
func Steps(p Plan) []Step {
	if len(p.Nodes) == 0 {
		return nil
	}
	out := make([]Step, 0, len(p.Nodes)*9)
	step := func(n, a, target, peer string) {
		out = append(out, Step{UpgradeID: p.ID, ID: len(out), NodeID: n, Action: a, TargetDigest: target, PeerNode: peer, Status: "pending"})
	}
	add := func(n, a, peer string) { step(n, a, p.ReleaseDigest, peer) }
	// A gateway rollback finds the previous binaries installed.
	prepare, swap := "prepare", ""
	switch p.Strategy {
	case GatewayRolling:
		prepare, swap = "gw-prepare", "gw-swap"
	case GatewayRollback:
		prepare, swap = "", "gw-revert"
	}
	for _, n := range p.Nodes {
		if prepare != "" {
			add(n, prepare, "")
		}
	}
	if rollingStrategy(p.Strategy) {
		// Followers first, the primary last: it coordinates the plan and the
		// plugins, so it is the node whose core restarts with the most
		// certainty that the target release serves.
		order := append(append([]string{}, p.Nodes[1:]...), p.Nodes[0])
		for _, n := range order {
			add(n, "yield", "")
			add(n, "stop", "")
			if swap != "" {
				add(n, swap, "")
			}
			for _, a := range []string{"start", "admit", "local"} {
				step(n, a, p.CoreDigest(), "")
			}
			if swap != "" {
				add(n, "gw-confirm", "")
			}
			add(n, "resume", "")
		}
		return out
	}
	primary := p.Nodes[0]
	for _, n := range p.Nodes[1:] {
		add(n, "redirect", primary)
		add(n, "stop", "")
	}
	add(primary, "maintenance", "")
	add(primary, "stop", "")
	add(primary, "start-primary", "")
	add(primary, "admit", "")
	add(primary, "local", "")
	for _, n := range p.Nodes[1:] {
		add(n, "redirect", primary)
		add(n, "start", "")
		add(n, "admit", "")
		add(n, "local", "")
	}
	return out
}

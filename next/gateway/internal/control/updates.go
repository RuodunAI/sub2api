package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/release"
	"github.com/Sub2API-Devs/sup2api/next/gateway/internal/updatesource"
	rc "github.com/Sub2API-Devs/sup2api/next/runtime-contract"
	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/secret"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The signed manifests a release of each line carries.
const (
	coreManifestAsset    = "next-core-manifest.json"
	gatewayManifestAsset = "next-gateway-manifest.json"
)

const (
	// updateCheckInterval is how old the stored results of the background
	// check may grow; updateCheckPoll is how often the primary looks.
	updateCheckInterval = 6 * time.Hour
	updateCheckPoll     = time.Minute
	// updateCheckDebounce is how recent a result answers a forced check.
	updateCheckDebounce = 30 * time.Second
	// updateCheckBudget bounds one check under the cluster lock, and so a
	// forced check. applyBudget bounds an apply's import and preflight; its
	// plan creation (at most the 30 s cluster change lock) then runs to its
	// end even if the caller left. The core's bridge waits longer than
	// either (server/internal/updater slowBridgeTimeout).
	updateCheckBudget = 3 * time.Minute
	applyBudget       = 2 * time.Minute
)

// UpdateService holds what update checks and imports need beyond the store.
type UpdateService struct {
	Manager *release.Manager // trusted keys and this node's platform
	// Cipher seals the source secret; nil without SUB2API_MASTER_KEY.
	Cipher  *secret.Cipher
	Sources updatesource.Options
	// sourceChanged wakes RunUpdateChecks after a source change.
	sourceChanged chan struct{}
}

func NewUpdateService(m *release.Manager, cipher *secret.Cipher, allowPrivate bool) *UpdateService {
	return &UpdateService{Manager: m, Cipher: cipher, Sources: updatesource.Options{AllowPrivate: allowPrivate}, sourceChanged: make(chan struct{}, 1)}
}

var (
	errMasterKeyMissing = errors.New("SUB2API_MASTER_KEY is not available to the gateway, so a source secret cannot be stored")
	errReleaseNotFound  = errors.New("the update source has no such stable release")
	errUpdatesOff       = errors.New("update sources are unavailable on this gateway")
)

// UpdateSource is the cluster's update source as the console sees it.
type UpdateSource struct {
	Enabled           bool   `json:"enabled"`
	Kind              string `json:"kind"`
	Repository        string `json:"repository"`
	URL               string `json:"url"`
	HasSecret         bool   `json:"has_secret"`
	DefaultRepository string `json:"default_repository"`
	Revision          int64  `json:"revision"`
}

// sourceRow is the source as stored on the cluster row.
type sourceRow struct {
	enabled         bool
	kind, repo, url string
	secret          []byte
	revision        int64
}

func (s *Store) readSource(q *gorm.DB) (r sourceRow, err error) {
	err = q.Select("update_check_enabled,update_source_kind,update_repository,update_url,update_secret_enc,update_source_revision").Row().
		Scan(&r.enabled, &r.kind, &r.repo, &r.url, &r.secret, &r.revision)
	return
}

func (r sourceRow) view() UpdateSource {
	return UpdateSource{Enabled: r.enabled, Kind: r.kind, Repository: r.repo, URL: r.url, HasSecret: len(r.secret) > 0,
		DefaultRepository: updatesource.DefaultRepository, Revision: r.revision}
}

// locator names where the source's releases live: the kind and its
// repository or url. Asset references and the secret belong to it.
func (r sourceRow) locator() string {
	if r.kind == string(updatesource.URL) {
		return r.kind + ":" + r.url
	}
	return r.kind + ":" + r.repo
}

// aad binds a sealed secret to the source it was given for.
func (r sourceRow) aad() []byte { return []byte("updater:source-secret:" + r.locator()) }

// config opens the secret; it fails when the secret cannot be read.
func (r sourceRow) config(c *secret.Cipher) (updatesource.Config, error) {
	cfg := updatesource.Config{Enabled: r.enabled, Kind: updatesource.Kind(r.kind), Repository: r.repo, URL: r.url, Revision: r.revision}
	if len(r.secret) == 0 {
		return cfg, nil
	}
	if c == nil {
		return cfg, errors.New("the update source secret cannot be read without SUB2API_MASTER_KEY")
	}
	plain, err := c.Decrypt(r.secret, r.aad())
	if err != nil {
		return cfg, errors.New("the update source secret cannot be decrypted with this master key")
	}
	cfg.Secret = string(plain)
	return cfg, nil
}

// SourceRequest is the PUT body: Secret nil keeps the secret (unless the
// source changes), "" clears it, anything else replaces it.
type SourceRequest struct {
	Enabled    bool    `json:"enabled"`
	Kind       string  `json:"kind"`
	Repository string  `json:"repository"`
	URL        string  `json:"url"`
	Secret     *string `json:"secret"`
}

// fieldError rejects one request field.
type fieldError struct{ field, message string }

func (e *fieldError) Error() string { return e.message }

// normalize validates the request into the row it stores (without secret).
// The locator of the chosen kind is required; the other may stay empty.
func (q SourceRequest) normalize() (sourceRow, error) {
	r := sourceRow{enabled: q.Enabled, kind: q.Kind}
	github, index := q.Kind == string(updatesource.GitHub), q.Kind == string(updatesource.URL)
	if !github && !index {
		return r, &fieldError{"kind", "kind must be github or url"}
	}
	var err error
	if github || q.Repository != "" {
		if r.repo, err = updatesource.NormalizeRepository(q.Repository); err != nil {
			return r, &fieldError{"repository", err.Error()}
		}
	}
	if index || q.URL != "" {
		if r.url, err = updatesource.NormalizeURL(q.URL); err != nil {
			return r, &fieldError{"url", err.Error()}
		}
	}
	if q.Secret != nil && *q.Secret != "" && !validSecret(*q.Secret) {
		return r, &fieldError{"secret", "secret must be 1-4096 printable characters without spaces"}
	}
	return r, nil
}

func validSecret(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return len(s) > 0 && len(s) <= 4096
}

// The secret_change of a SourceUpdate.
const (
	secretSet       = "set"
	secretCleared   = "cleared"
	secretUnchanged = "unchanged"
)

// SourceUpdate is the PUT answer: the stored source, whether the request
// changed it, and what became of its secret (set, cleared or unchanged; a
// moved source drops a secret not given anew), for the core's audit.
type SourceUpdate struct {
	UpdateSource
	Changed      bool   `json:"changed"`
	SecretChange string `json:"secret_change"`
}

// SetUpdateSource stores the source. A changed locator (the kind, and the
// repository or url it reads) drops a secret not given anew, and any change
// raises the revision, which retires every stored check result.
func (s *Store) SetUpdateSource(ctx context.Context, q SourceRequest) (out SourceUpdate, err error) {
	next, err := q.normalize()
	if err != nil {
		return out, err
	}
	setSecret := q.Secret != nil && *q.Secret != ""
	if setSecret && (s.Updates == nil || s.Updates.Cipher == nil) {
		return out, errMasterKeyMissing
	}
	out.SecretChange = secretUnchanged
	err = s.withTx(ctx, func(tx *gorm.DB) error {
		row, err := s.readSource(s.clusterRow(tx).Clauses(forUpdate()))
		if err != nil {
			return err
		}
		next.secret, next.revision = row.secret, row.revision
		switch {
		case setSecret:
			if next.secret, err = s.Updates.Cipher.Encrypt([]byte(*q.Secret), next.aad()); err != nil {
				return err
			}
			out.SecretChange = secretSet
		case (q.Secret != nil || row.locator() != next.locator()) && len(row.secret) > 0:
			next.secret = nil
			out.SecretChange = secretCleared
		}
		out.Changed = out.SecretChange != secretUnchanged || row.enabled != next.enabled || row.kind != next.kind || row.repo != next.repo || row.url != next.url
		if !out.Changed {
			return nil
		}
		next.revision++
		return s.clusterRow(tx).Updates(map[string]any{"update_check_enabled": next.enabled, "update_source_kind": next.kind,
			"update_repository": next.repo, "update_url": next.url, "update_secret_enc": next.secret,
			"update_source_revision": gorm.Expr("update_source_revision+1")}).Error
	})
	if err == nil && out.Changed && next.enabled && s.Updates != nil {
		// RunUpdateChecks checks the new source at once, so the console
		// shows its result without waiting for the primary's next look.
		select {
		case s.Updates.sourceChanged <- struct{}{}:
		default:
		}
	}
	out.UpdateSource = next.view()
	return out, err
}

// RunUpdateChecks checks the update source on the upgrade primary: a minute
// after start, then whenever the stored results are older than
// updateCheckInterval or describe another source or baseline. A source
// changed through this node is checked here at once, primary or not.
func (s *Store) RunUpdateChecks(ctx context.Context, node string) {
	tick := time.NewTicker(updateCheckPoll)
	defer tick.Stop()
	var changed <-chan struct{}
	if s.Updates != nil {
		changed = s.Updates.sourceChanged
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if primary, _, _, err := s.ClusterState(ctx); err != nil || primary != node {
				continue
			}
		case <-changed:
		}
		if due, err := s.updateCheckDue(ctx, updateCheckInterval); err != nil || !due {
			continue
		}
		if err := s.CheckUpdates(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("update check failed", "err", err)
		}
	}
}

// updateCheckDue reports whether an enabled source lacks results for its
// revision and the baseline that are younger than age.
func (s *Store) updateCheckDue(ctx context.Context, age time.Duration) (bool, error) {
	db := s.db(ctx)
	row, err := s.readSource(s.clusterRow(db))
	if err != nil || !row.enabled {
		return false, err
	}
	_, baseline, _, err := s.ClusterState(ctx)
	if err != nil {
		return false, err
	}
	var fresh int64
	err = db.Model(&updateCheckRecord{}).Where("cluster_id = ? AND source_revision = ? AND baseline = ? AND checked_at > ?",
		s.Cluster, row.revision, baseline, time.Now().Add(-age)).Count(&fresh).Error
	return fresh < 2, err
}

// CheckUpdates checks the source once under the cluster's update-check lock;
// while another node checks, it returns at once.
func (s *Store) CheckUpdates(ctx context.Context) error {
	if s.Updates == nil {
		return errUpdatesOff
	}
	_, err := s.Locks.WithLock(ctx, "system:update-check:"+s.Cluster, updateCheckBudget, s.checkUpdates)
	return err
}

// lineOutcome is one line's check: a result, or why there is none.
type lineOutcome struct {
	line   string
	result any
	err    error
}

func (s *Store) checkUpdates(ctx context.Context) error {
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil || !row.enabled {
		return err
	}
	_, baseline, _, err := s.ClusterState(ctx)
	if err != nil {
		return err
	}
	current, err := s.Release(ctx, baseline)
	if err != nil {
		return err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return err
	}
	outcomes := s.Updates.check(ctx, row, current, nodes)
	if err = ctx.Err(); err != nil {
		return err
	}
	// A source or baseline changed meanwhile gets a check of its own.
	now, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		return err
	}
	_, nowBaseline, _, err := s.ClusterState(ctx)
	if err != nil || now.revision != row.revision || nowBaseline != baseline {
		return err
	}
	for _, o := range outcomes {
		if err = s.recordCheck(ctx, o, row.revision, baseline); err != nil {
			return err
		}
	}
	return nil
}

// recordCheck stores a line's outcome. A failure keeps the last successful
// result of the same source and baseline, with the error beside it.
func (s *Store) recordCheck(ctx context.Context, o lineOutcome, revision int64, baseline string) error {
	now := time.Now().UTC()
	rec := updateCheckRecord{ClusterID: s.Cluster, Line: o.line, SourceRevision: revision, Baseline: baseline, CheckedAt: now}
	set := clause.AssignmentColumns([]string{"source_revision", "baseline", "result", "checked_at", "last_success_at"})
	if o.err == nil {
		rec.Result, _ = json.Marshal(o.result)
		rec.LastSuccessAt = &now
	} else {
		rec.Result, _ = json.Marshal(map[string]string{"error": o.err.Error()})
		const same = "CASE WHEN updater.update_checks.source_revision=EXCLUDED.source_revision AND updater.update_checks.baseline=EXCLUDED.baseline THEN "
		set = append(clause.AssignmentColumns([]string{"source_revision", "baseline", "checked_at"}), clause.Assignments(map[string]any{
			"result":          gorm.Expr(same + "updater.update_checks.result || EXCLUDED.result ELSE EXCLUDED.result END"),
			"last_success_at": gorm.Expr(same + "updater.update_checks.last_success_at END"),
		})...)
	}
	return s.db(ctx).Clauses(clause.OnConflict{Columns: columns("cluster_id", "line"), DoUpdates: set}).Create(&rec).Error
}

// lineResult is the newest release of a line and the verdict on it, as
// stored in updater.update_checks.result.
type lineResult struct {
	LatestVersion string `json:"latest_version"`
	Compatible    bool   `json:"compatible"`
	// ReasonCode classifies Reason for the console: reasonNoRelease,
	// reasonVersionConflict or reasonIncompatible (Reason then lists the
	// compatibility findings).
	ReasonCode  string `json:"reason_code,omitempty"`
	Reason      string `json:"reason"`
	Tag         string `json:"tag"`
	Notes       string `json:"notes"`
	Page        string `json:"page"`
	PublishedAt string `json:"published_at"`
	// Error is why the latest check failed; the other fields then still
	// describe the last successful one.
	Error string `json:"error"`
}
type coreResult struct {
	lineResult
	Strategy string `json:"strategy"`
}
type gatewayResult struct {
	lineResult
	SelfUpdate bool   `json:"self_update"`
	Image      string `json:"image"`
}

func describe(l updatesource.Listing) lineResult {
	r := lineResult{LatestVersion: l.Version, Tag: l.Tag, Notes: l.Notes, Page: l.Page}
	if !l.PublishedAt.IsZero() {
		r.PublishedAt = l.PublishedAt.UTC().Format(time.RFC3339)
	}
	return r
}

const (
	reasonNoRelease       = "no_release"
	reasonVersionConflict = "version_conflict"
	reasonIncompatible    = "incompatible"
)

func (u *UpdateService) check(ctx context.Context, row sourceRow, current Release, nodes []Node) []lineOutcome {
	src, err := u.source(row)
	var listings []updatesource.Listing
	if err == nil {
		listings, err = src.Releases(ctx)
	}
	if err != nil {
		return []lineOutcome{{line: updatesource.LineCore, err: err}, {line: updatesource.LineGateway, err: err}}
	}
	core, coreErr := u.checkCore(ctx, src, listings, current)
	gateway, gatewayErr := u.checkGateway(ctx, src, listings, current, nodes)
	return []lineOutcome{{updatesource.LineCore, core, coreErr}, {updatesource.LineGateway, gateway, gatewayErr}}
}

func (u *UpdateService) source(row sourceRow) (updatesource.Source, error) {
	cfg, err := row.config(u.Cipher)
	if err != nil {
		return nil, err
	}
	return updatesource.New(cfg, u.Sources)
}

func (u *UpdateService) checkCore(ctx context.Context, src updatesource.Source, ls []updatesource.Listing, current Release) (coreResult, error) {
	l, ok := updatesource.Latest(ls, updatesource.LineCore)
	if !ok {
		return coreResult{lineResult: lineResult{ReasonCode: reasonNoRelease, Reason: "the update source has no stable core release"}}, nil
	}
	target, err := u.verifyCore(ctx, src, l)
	if err != nil {
		return coreResult{}, err
	}
	r := coreResult{lineResult: describe(l), Strategy: target.Manifest.Strategy}
	switch blockers := Compatibility(current, target); {
	case target.Digest == current.Digest:
		r.Compatible = true
	case strings.TrimPrefix(current.Manifest.CoreVersion, "v") == l.Version:
		r.ReasonCode, r.Reason = reasonVersionConflict, "the latest release uses the installed version number with a different signed identity"
	case len(blockers) > 0:
		r.ReasonCode, r.Reason = reasonIncompatible, strings.Join(blockers, "; ")
	default:
		r.Compatible = true
	}
	return r, nil
}

// verifyCore checks a core listing: its signed manifest, the version it
// binds, this node's platform and a bundle asset of the signed size for
// every platform, so that a heterogeneous cluster can install it.
func (u *UpdateService) verifyCore(ctx context.Context, src updatesource.Source, l updatesource.Listing) (Release, error) {
	signed, err := openManifest(ctx, src, l, coreManifestAsset)
	if err != nil {
		return Release{}, err
	}
	manifest, digest, err := release.Verify(signed, u.Manager.TrustedKeys)
	if err != nil {
		return Release{}, fmt.Errorf("%s: %w", l.Tag, err)
	}
	if strings.TrimPrefix(manifest.CoreVersion, "v") != l.Version {
		return Release{}, fmt.Errorf("%s: the signed core version differs from the tag", l.Tag)
	}
	if _, err = u.Manager.ValidatePlatform(manifest); err != nil {
		return Release{}, fmt.Errorf("%s: %w", l.Tag, err)
	}
	for _, p := range manifest.Platforms {
		a, ok := l.Assets[p.BundleDigest+".tar.gz"]
		if !release.ValidDigest(p.BundleDigest) || p.BundleBytes <= 0 || p.BundleBytes > 16<<30 || !ok || a.Size != p.BundleBytes {
			return Release{}, fmt.Errorf("%s: a signed bundle is missing or differs in size", l.Tag)
		}
	}
	return Release{Digest: digest, Manifest: manifest, Signed: signed, SourceAssets: l.Assets}, nil
}

func (u *UpdateService) checkGateway(ctx context.Context, src updatesource.Source, ls []updatesource.Listing, current Release, nodes []Node) (gatewayResult, error) {
	l, ok := updatesource.Latest(ls, updatesource.LineGateway)
	if !ok {
		return gatewayResult{lineResult: lineResult{ReasonCode: reasonNoRelease, Reason: "the update source has no stable gateway release"}}, nil
	}
	g, err := u.verifyGateway(ctx, src, l)
	if err != nil {
		return gatewayResult{}, err
	}
	m := g.Manifest
	r := gatewayResult{lineResult: describe(l)}
	if len(m.Images) > 0 {
		r.Image = m.Images[0].Ref + "@" + m.Images[0].Digest
	}
	if reasons := gatewayCompatibility(m, current, nodes); len(reasons) > 0 {
		r.ReasonCode, r.Reason = reasonIncompatible, strings.Join(reasons, "; ")
	} else {
		r.Compatible = true
	}
	return r, nil
}

// verifyGateway checks a gateway listing: its signed manifest, the version
// it binds and an archive asset of the signed size for every platform.
func (u *UpdateService) verifyGateway(ctx context.Context, src updatesource.Source, l updatesource.Listing) (GatewayRelease, error) {
	signed, err := openManifest(ctx, src, l, gatewayManifestAsset)
	if err != nil {
		return GatewayRelease{}, err
	}
	m, digest, err := release.VerifyGateway(signed, u.Manager.TrustedKeys)
	if err != nil {
		return GatewayRelease{}, fmt.Errorf("%s: %w", l.Tag, err)
	}
	if m.GatewayVersion != l.Version {
		return GatewayRelease{}, fmt.Errorf("%s: the signed gateway version differs from the tag", l.Tag)
	}
	for _, p := range m.Platforms {
		if a, ok := l.Assets[p.Archive]; !ok || a.Size != p.ArchiveBytes {
			return GatewayRelease{}, fmt.Errorf("%s: a signed gateway archive is missing or differs in size", l.Tag)
		}
	}
	return GatewayRelease{Digest: digest, Manifest: m, Signed: signed, SourceAssets: l.Assets}, nil
}

// gatewayCompatibility says why m cannot replace the cluster's gateways: it
// must speak their peer protocol, a runtime protocol the installed core
// accepts, and have an archive for every enabled node's platform.
func gatewayCompatibility(m rc.GatewayManifest, current Release, nodes []Node) []string {
	var out []string
	if m.PeerProtocol != PeerProtocol {
		out = append(out, fmt.Sprintf("the release speaks peer protocol %d, the cluster %d", m.PeerProtocol, PeerProtocol))
	}
	if !current.Manifest.ShellProtocol.Contains(m.RuntimeProtocol) {
		out = append(out, fmt.Sprintf("the installed core does not accept runtime protocol %d", m.RuntimeProtocol))
	}
	for _, n := range nodes {
		found := false
		for _, p := range m.Platforms {
			found = found || (p.OS == n.OS && p.Arch == n.Arch)
		}
		if n.Enabled && !found {
			out = append(out, fmt.Sprintf("no archive for node %s (%s/%s)", n.ID, n.OS, n.Arch))
		}
	}
	return out
}

// openManifest downloads and decodes a listing's signed manifest asset.
func openManifest(ctx context.Context, src updatesource.Source, l updatesource.Listing, name string) (rc.SignedManifest, error) {
	var s rc.SignedManifest
	a, ok := l.Assets[name]
	if !ok {
		return s, fmt.Errorf("%s has no %s", l.Tag, name)
	}
	ctx, cancel := context.WithTimeout(ctx, release.DefaultManifestTimeout)
	defer cancel()
	body, err := src.Open(ctx, a, release.MaxManifestBytes*2)
	if err != nil {
		return s, fmt.Errorf("%s: %w", l.Tag, err)
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return s, fmt.Errorf("%s: unreadable %s", l.Tag, name)
	}
	return s, nil
}

// UpdateCheck is GET /system/update-check.
type UpdateCheck struct {
	Enabled bool         `json:"enabled"`
	Source  sourceRef    `json:"source"`
	Core    CoreCheck    `json:"core"`
	Gateway GatewayCheck `json:"gateway"`
}
type sourceRef struct {
	Kind       string `json:"kind"`
	Repository string `json:"repository"`
	URL        string `json:"url"`
}
type checkTimes struct {
	CheckedAt     *time.Time `json:"checked_at"`
	LastSuccessAt *time.Time `json:"last_success_at"`
}
type CoreCheck struct {
	CurrentVersion string `json:"current_version"`
	HasUpdate      bool   `json:"has_update"`
	coreResult
	checkTimes
}
type GatewayCheck struct {
	Nodes     []NodeVersion `json:"nodes"`
	HasUpdate bool          `json:"has_update"`
	gatewayResult
	checkTimes
}
type NodeVersion struct {
	NodeID  string `json:"node_id"`
	Version string `json:"version"`
}

// UpdateCheck reads the stored results of the current source and baseline;
// whether an update applies is decided against the versions installed now.
func (s *Store) UpdateCheck(ctx context.Context) (UpdateCheck, error) {
	var out UpdateCheck
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		return out, err
	}
	_, baseline, _, err := s.ClusterState(ctx)
	if err != nil {
		return out, err
	}
	current, err := s.Release(ctx, baseline)
	if err != nil {
		return out, err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return out, err
	}
	var rows []updateCheckRecord
	if err = s.db(ctx).Where("cluster_id = ? AND source_revision = ? AND baseline = ?", s.Cluster, row.revision, baseline).Find(&rows).Error; err != nil {
		return out, err
	}
	out.Enabled, out.Source = row.enabled, sourceRef{row.kind, row.repo, row.url}
	out.Core.CurrentVersion = current.Manifest.CoreVersion
	out.Gateway.Nodes = []NodeVersion{}
	for _, n := range nodes {
		out.Gateway.Nodes = append(out.Gateway.Nodes, NodeVersion{n.ID, n.GatewayVersion})
	}
	for _, r := range rows {
		times := checkTimes{&r.CheckedAt, r.LastSuccessAt}
		switch r.Line {
		case updatesource.LineCore:
			out.Core.checkTimes = times
			err = json.Unmarshal(r.Result, &out.Core.coreResult)
		case updatesource.LineGateway:
			out.Gateway.checkTimes = times
			err = json.Unmarshal(r.Result, &out.Gateway.gatewayResult)
		}
		if err != nil {
			return out, err
		}
	}
	out.Core.HasUpdate = older(out.Core.CurrentVersion, out.Core.LatestVersion)
	out.Gateway.SelfUpdate = enabledNodes(nodes) > 0
	for _, n := range nodes {
		out.Gateway.HasUpdate = out.Gateway.HasUpdate || (n.Enabled && older(n.GatewayVersion, out.Gateway.LatestVersion))
		out.Gateway.SelfUpdate = out.Gateway.SelfUpdate && (!n.Enabled || (n.SelfUpdate && n.GatewayVersion != ""))
	}
	return out, nil
}

// older reports whether version predates latest; a version that is not
// semantic (a gateway too old to report one) predates any.
func older(version, latest string) bool {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false
	}
	v, err := semver.NewVersion(version)
	return err != nil || v.LessThan(l)
}

// ApplyRequest is POST /system/updates/apply. IdempotencyKey is optional: a
// repeated key returns the plan it created.
type ApplyRequest struct {
	Line           string `json:"line"`
	Version        string `json:"version"`
	Confirm        bool   `json:"confirm"`
	IdempotencyKey string `json:"idempotency_key"`
}
type ApplyResult struct {
	Status  string   `json:"status"` // started | needs_confirmation
	PlanID  string   `json:"plan_id,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	// ReasonCodes are the Reasons as Issues, in the same order.
	ReasonCodes []Issue    `json:"reason_codes,omitempty"`
	Preflight   *Preflight `json:"preflight,omitempty"`
}

// blockedError is a preflight with blockers.
type blockedError struct{ preflight Preflight }

func (e *blockedError) Error() string { return strings.Join(e.preflight.Blockers, "; ") }

// Apply imports release version of q.Line from the update source and
// starts its plan. A core plan starts at once when it rolls; a primary-first
// one, whose followers stop serving while the primary migrates, waits for
// confirm. A gateway plan waits for confirm when a single node has nobody
// to hand its traffic to.
func (s *Store) Apply(ctx context.Context, q ApplyRequest, actor string) (ApplyResult, error) {
	var digest string
	var confirm func(Preflight, []Node) []Issue
	switch q.Line {
	case updatesource.LineCore:
		r, err := s.importCore(ctx, q.Version)
		if err != nil {
			return ApplyResult{}, err
		}
		digest = r.Digest
		confirm = func(pf Preflight, nodes []Node) []Issue {
			if pf.Strategy == Rolling {
				return nil
			}
			return rollingBlockers(r, nodes, 2)
		}
	case updatesource.LineGateway:
		r, err := s.importGateway(ctx, q.Version)
		if err != nil {
			return ApplyResult{}, err
		}
		digest = r.Digest
		confirm = func(_ Preflight, nodes []Node) []Issue {
			if enabledNodes(nodes) == 1 {
				return []Issue{{Code: issueSingleNode, Params: map[string]string{"seconds": "10"}, Message: "single node: about 10 s interruption"}}
			}
			return nil
		}
	default:
		return ApplyResult{}, errors.New("line must be core or gateway")
	}
	if q.IdempotencyKey != "" {
		var id, prior string
		err := s.idempotentUpgrade(s.db(ctx), q.IdempotencyKey).Select("id,release_digest").Row().Scan(&id, &prior)
		if err == nil && prior != digest {
			return ApplyResult{}, ErrConflict
		}
		if err == nil {
			return ApplyResult{Status: "started", PlanID: id}, nil
		}
		if !gormpg.IsNoRows(err) {
			return ApplyResult{}, err
		}
	}
	pf, err := s.Preflight(ctx, digest)
	if err != nil {
		return ApplyResult{}, err
	}
	if len(pf.Blockers) > 0 {
		return ApplyResult{}, &blockedError{pf}
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return ApplyResult{}, err
	}
	if reasons := confirm(pf, nodes); len(reasons) > 0 && !q.Confirm {
		return ApplyResult{Status: "needs_confirmation", Reasons: messages(reasons), ReasonCodes: reasons, Preflight: &pf}, nil
	}
	key := q.IdempotencyKey
	if key == "" {
		key = "apply:" + digest + ":" + strconv.FormatInt(pf.ExpectedRevision, 10)
	}
	// Once started, the plan's creation outlives the request, so that its
	// answer, and the core's audit of it, is not lost to a caller leaving.
	p, err := s.Create(context.WithoutCancel(ctx), digest, pf.ExpectedRevision, key, actor)
	return ApplyResult{Status: "started", PlanID: p.ID}, err
}

func enabledNodes(nodes []Node) int {
	n := 0
	for _, node := range nodes {
		if node.Enabled {
			n++
		}
	}
	return n
}

// importCore stores a verified core release of the source.
func (s *Store) importCore(ctx context.Context, version string) (Release, error) {
	src, row, l, err := s.sourceListing(ctx, updatesource.LineCore, version)
	if err != nil {
		return Release{}, err
	}
	r, err := s.Updates.verifyCore(ctx, src, l)
	if err != nil {
		return r, err
	}
	r.SourceLocator = row.locator()
	if err = s.storeImported(ctx, row.revision, newReleaseRecord(r)); err != nil {
		return r, err
	}
	return s.Release(ctx, r.Digest)
}

// importGateway stores a verified gateway release of the source.
func (s *Store) importGateway(ctx context.Context, version string) (GatewayRelease, error) {
	src, row, l, err := s.sourceListing(ctx, updatesource.LineGateway, version)
	if err != nil {
		return GatewayRelease{}, err
	}
	r, err := s.Updates.verifyGateway(ctx, src, l)
	if err != nil {
		return r, err
	}
	m, _ := json.Marshal(r.Manifest)
	signed, _ := json.Marshal(r.Signed)
	assets, _ := json.Marshal(r.SourceAssets)
	rec := &releaseRecord{Digest: r.Digest, ReleaseID: r.Manifest.ReleaseID, Manifest: m, SignedManifest: signed, Line: updatesource.LineGateway,
		SourceAssets: assets, SourceLocator: row.locator()}
	if err = s.storeImported(ctx, row.revision, rec); err != nil {
		return r, err
	}
	return s.GatewayRelease(ctx, r.Digest)
}

// sourceListing finds the stable release version of line at the configured
// source, read at the revision it returns.
func (s *Store) sourceListing(ctx context.Context, line, version string) (updatesource.Source, sourceRow, updatesource.Listing, error) {
	if s.Updates == nil {
		return nil, sourceRow{}, updatesource.Listing{}, errUpdatesOff
	}
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		return nil, row, updatesource.Listing{}, err
	}
	src, err := s.Updates.source(row)
	if err != nil {
		return nil, row, updatesource.Listing{}, err
	}
	ls, err := src.Releases(ctx)
	if err != nil {
		return nil, row, updatesource.Listing{}, err
	}
	for _, l := range ls {
		if l.Line == line && l.Version == version {
			return src, row, l, nil
		}
	}
	return nil, row, updatesource.Listing{}, errReleaseNotFound
}

// storeImported stores rec, a release verified from the source at revision,
// unless the source changed meanwhile; a release imported before from a
// source takes the new asset references and locator.
func (s *Store) storeImported(ctx context.Context, revision int64, rec *releaseRecord) error {
	return s.withTx(ctx, func(tx *gorm.DB) error {
		var now int64
		if err := s.clusterRow(tx).Clauses(forShare()).Select("update_source_revision").Row().Scan(&now); err != nil {
			return err
		}
		if now != revision {
			return ErrConflict
		}
		return tx.Clauses(clause.OnConflict{Columns: columns("digest"), DoUpdates: clause.AssignmentColumns([]string{"source_assets", "source_locator"}),
			Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "updater.releases.bundle_base=''"}}}}).Create(rec).Error
	})
}

// OpenSourceAsset opens an asset of r, a release imported from the update
// source, through the source as configured now: it must still read the
// locator r was imported from, since asset references and the secret belong
// to it. Other changes (the switch, a new secret) leave running plans alone.
func (s *Store) OpenSourceAsset(ctx context.Context, r Release, name string, max int64) (io.ReadCloser, error) {
	return s.openAsset(ctx, r.SourceAssets, r.SourceLocator, name, max)
}

// openAsset opens asset name of a release imported from locator with assets.
func (s *Store) openAsset(ctx context.Context, assets map[string]updatesource.Asset, locator, name string, max int64) (io.ReadCloser, error) {
	a, ok := assets[name]
	if !ok {
		return nil, fmt.Errorf("release has no source asset %s", name)
	}
	if s.Updates == nil {
		return nil, errUpdatesOff
	}
	row, err := s.readSource(s.clusterRow(s.db(ctx)))
	if err != nil {
		return nil, err
	}
	if row.locator() != locator {
		return nil, errors.New("the update source moved after this release was imported; apply it again")
	}
	src, err := s.Updates.source(row)
	if err != nil {
		return nil, err
	}
	return src.Open(ctx, a, max)
}

func (s *Store) updateHandlers(mux *http.ServeMux, reply func(http.ResponseWriter, any, error)) {
	mux.HandleFunc("GET /system/update-source", func(w http.ResponseWriter, r *http.Request) {
		row, err := s.readSource(s.clusterRow(s.db(r.Context())))
		reply(w, row.view(), err)
	})
	mux.HandleFunc("PUT /system/update-source", func(w http.ResponseWriter, r *http.Request) {
		var q SourceRequest
		if err := decode(w, r, &q); err != nil {
			invalid(w, "", err.Error())
			return
		}
		v, err := s.SetUpdateSource(r.Context(), q)
		var fe *fieldError
		switch {
		case errors.As(err, &fe):
			invalid(w, fe.field, fe.message)
		case errors.Is(err, errMasterKeyMissing):
			writeError(w, http.StatusBadRequest, "master_key_missing", err.Error(), nil)
		default:
			reply(w, v, err)
		}
	})
	mux.HandleFunc("GET /system/update-check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "true" {
			due, err := s.updateCheckDue(r.Context(), updateCheckDebounce)
			if err == nil && due {
				err = s.CheckUpdates(r.Context())
			}
			if err != nil {
				reply(w, nil, err)
				return
			}
		}
		v, err := s.UpdateCheck(r.Context())
		reply(w, v, err)
	})
	mux.HandleFunc("POST /system/updates/apply", func(w http.ResponseWriter, r *http.Request) {
		var q ApplyRequest
		if err := decode(w, r, &q); err != nil {
			invalid(w, "", err.Error())
			return
		}
		if q.Line != updatesource.LineCore && q.Line != updatesource.LineGateway {
			invalid(w, "line", "line must be core or gateway")
			return
		}
		if v, err := semver.StrictNewVersion(q.Version); err != nil || v.Prerelease() != "" {
			invalid(w, "version", "version must be a stable semantic version")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), applyBudget)
		defer cancel()
		v, err := s.Apply(ctx, q, r.Header.Get("X-Updater-Actor"))
		var blocked *blockedError
		switch {
		case errors.As(err, &blocked):
			writeError(w, http.StatusConflict, "preflight_blocked", err.Error(), map[string]any{"blockers": blocked.preflight.Blockers,
				"blocker_codes": blocked.preflight.BlockerCodes, "preflight": blocked.preflight})
		case errors.Is(err, errReleaseNotFound):
			writeError(w, http.StatusNotFound, "release_not_found", err.Error(), nil)
		default:
			reply(w, v, err)
		}
	})
}

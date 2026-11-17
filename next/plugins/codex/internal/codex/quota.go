package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// Usage windows of codex_device accounts (CONTRACTS §44.3, §105,
// docs/PLUGIN-CODEX.md §13). The ChatGPT Codex backend reports them only in
// the x-codex-{primary,secondary}-* headers of its answers; manifest.json
// declares those headers by window length (windowMinutes), and the core
// samples them from every response, so the plugin sends no usage query.
// ClassifyError reads the same declarations from the plugin's own manifest
// (withManifest): the header names live in manifest.json only.
// chatgpt_siwc accounts have no quota: Sign in with ChatGPT documents no
// usage headers.

// withManifest makes ClassifyError read the codex_device quota headers that
// manifestJSON, the plugin's own manifest, declares.
func (p *Plugin) withManifest(manifestJSON []byte) (*Plugin, error) {
	var m manifest.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("codex: read the manifest: %w", err)
	}
	for _, t := range m.AccountTypes {
		if t.ID == TypeDevice && t.Quota != nil && len(t.Quota.Headers) > 0 {
			p.policy = newPolicy(t.Quota.Headers)
			return p, nil
		}
	}
	return nil, errors.New("codex: the manifest declares no codex_device quota headers")
}

// usedUpUntil returns the latest reset of the windows that headers (lower-
// cased names) show used up: at least 100 % with a reset after now. Any
// window counts. ok is false when no window is used up with a known reset.
func usedUpUntil(decls []manifest.QuotaHeader, headers map[string]string, now time.Time) (until time.Time, ok bool) {
	get := func(name string) string { return headers[strings.ToLower(name)] }
	for _, w := range manifest.ReadQuotaHeaders(decls, get, now) {
		if w.Utilization >= 100 && w.ResetsAt != nil && w.ResetsAt.After(now) && w.ResetsAt.After(until) {
			until = *w.ResetsAt
		}
	}
	return until, !until.IsZero()
}

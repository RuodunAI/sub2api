package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
)

// Usage windows of both account types (CONTRACTS §44.3, §105,
// docs/PLUGIN-CODEX.md §13). The ChatGPT Codex backend reports them only in
// the x-codex-{primary,secondary}-* headers of its answers; manifest.json
// declares those headers by window length (windowMinutes) on both types, and
// the core samples them from every response, so the plugin sends no usage
// query. ClassifyError reads the same declarations from the plugin's own
// manifest: the header names live in manifest.json only.

// quotaHeaders returns the codex_device quota headers that manifestJSON, the
// plugin's own manifest, declares.
func quotaHeaders(manifestJSON []byte) ([]manifest.QuotaHeader, error) {
	var m manifest.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, fmt.Errorf("codex: read the manifest: %w", err)
	}
	for _, t := range m.AccountTypes {
		if t.ID == TypeDevice && t.Quota != nil && len(t.Quota.Headers) > 0 {
			return t.Quota.Headers, nil
		}
	}
	return nil, errors.New("codex: the manifest declares no codex_device quota headers")
}

// usedUpUntil returns the latest reset of the usage windows a 429 shows used
// up: at least 100 % with a reset after now. Any window counts. ok is false
// for any other status and when no window is used up with a known reset.
func (p *Plugin) usedUpUntil(in *pluginv1.ClassifyErrorRequest, now time.Time) (until time.Time, ok bool) {
	if in.GetStatus() != http.StatusTooManyRequests {
		return until, false
	}
	get := func(name string) string { return in.GetHeaders()[strings.ToLower(name)] }
	for _, w := range manifest.ReadQuotaHeaders(p.quota, get, now) {
		if w.Utilization >= 100 && w.ResetsAt != nil && w.ResetsAt.After(now) && w.ResetsAt.After(until) {
			until = *w.ResetsAt
		}
	}
	return until, !until.IsZero()
}

// Package updater bridges authenticated console requests to the local gateway.
// Plugins cannot access this bridge through the Host API.
package updater

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// RegisterRoutes serves the console's update routes by forwarding them to
// the shell's management socket. token (config Managed.UpdaterToken) is sent
// as "Authorization: Bearer <token>" on every forwarded request.
func RegisterRoutes(r *httpapi.Router, socketPath, token string, db *store.DB) {
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}}
	registerRoutes(r, socketPath != "", client, token, db)
}

// How long a forwarded request waits for the gateway. The slowRoutes (a
// forced update check, an update apply) read the update source, listings and
// manifests, first: the gateway bounds them at 3 minutes (control
// updateCheckBudget; applyBudget plus plan creation), and slowBridgeTimeout
// outlasts that.
const (
	bridgeTimeout     = time.Minute
	slowBridgeTimeout = 4 * time.Minute
)

// slowRoutes are keyed by method and route pattern, like auditRules.
var slowRoutes = map[string]bool{
	"GET /api/v1/system/update-check":   true,
	"POST /api/v1/system/updates/apply": true,
}

func registerRoutes(r *httpapi.Router, available bool, client *http.Client, token string, db *store.DB) {
	handler := func(c *gin.Context) {
		if !available {
			c.JSON(503, gin.H{"error": gin.H{"code": "updater_unavailable", "message": "This node is not managed by the gateway"}})
			return
		}
		// Buffered so the audit can read what was asked; never logged.
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10))
		if err != nil {
			httpapi.Fail(c, core.ErrInvalidArgument)
			return
		}
		// A mutation, once forwarded, waits for its answer even when the
		// console leaves: what the gateway committed is always audited.
		ctx, timeout := c.Request.Context(), bridgeTimeout
		if c.Request.Method != http.MethodGet {
			ctx = context.WithoutCancel(ctx)
		}
		if slowRoutes[c.Request.Method+" "+c.FullPath()] {
			timeout = slowBridgeTimeout
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		path := strings.TrimPrefix(c.Request.URL.Path, "/api/v1")
		req, err := http.NewRequestWithContext(ctx, c.Request.Method, "http://shell"+path, bytes.NewReader(body))
		if err != nil {
			httpapi.Fail(c, err)
			return
		}
		req.URL.RawQuery = c.Request.URL.RawQuery
		req.Header.Set("Content-Type", "application/json")
		uid, _ := core.UserID(c.Request.Context())
		req.Header.Set("X-Updater-Actor", strconv.FormatInt(uid, 10))
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			c.JSON(503, gin.H{"error": gin.H{"code": "updater_unavailable", "message": "Local gateway is unavailable"}})
			return
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		if err != nil {
			httpapi.Fail(c, err)
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			auditForwarded(c, db, uid, body, payload)
		}
		c.Header("Content-Type", "application/json")
		c.Status(response.StatusCode)
		_, _ = c.Writer.Write(payload)
	}
	for _, path := range []string{"/system/releases", "/system/upgrades", "/system/upgrades/:id", "/system/upgrades/:id/events"} {
		r.Perm("GET", path, "system:update:read", handler)
	}
	r.Perm("POST", "/system/upgrades/preflight", "system:update:read", handler)
	// Running other code on every node: a step-up proof each time (§80.2).
	update := r.StepUp(core.StepUpSystemUpdate)
	update.Perm("POST", "/system/upgrades", "system:update:execute", handler)
	for _, action := range []string{"pause", "resume", "cancel", "rollback"} {
		update.Perm("POST", "/system/upgrades/:id/"+action, "system:update:recover", handler)
	}
	for _, action := range []string{"disable", "enable"} {
		update.Perm("POST", "/system/nodes/:id/"+action, "system:update:recover", handler)
	}
	// CPU offload is a system setting of the managed cluster.
	r.Perm("GET", "/system/offload", "settings:read", handler)
	r.Perm("PUT", "/system/offload", "settings:manage", handler)
	r.Perm("GET", "/system/update-source", "settings:read", handler)
	// The update source decides what code the next upgrade runs.
	update.Perm("PUT", "/system/update-source", "settings:manage", handler)
	r.Perm("GET", "/system/update-check", "system:update:read", handler)
	update.Perm("POST", "/system/updates/apply", "system:update:execute", handler)
}

// auditRule is the audit row of a successful forwarded mutation. details,
// when set, reads the forwarded request and the gateway's answer: it returns
// the target id ("" keeps the default), extra details, and false when the
// answer changed nothing.
type auditRule struct {
	action, targetType string
	details            func(request, response []byte) (string, map[string]any, bool)
}

// auditRules is keyed by method and route pattern.
var auditRules = func() map[string]auditRule {
	rules := map[string]auditRule{
		"PUT /api/v1/system/update-source":  {"system.update_source.update", "system", updateSourceDetails},
		"POST /api/v1/system/updates/apply": {"system.update.apply", "upgrade", applyDetails},
		"PUT /api/v1/system/offload":        {"system.offload.update", "system", nil},
		"POST /api/v1/system/upgrades":      {"system.upgrade.create", "upgrade", nil},
	}
	for _, action := range []string{"pause", "resume", "cancel", "rollback"} {
		rules["POST /api/v1/system/upgrades/:id/"+action] = auditRule{"system.upgrade." + action, "upgrade", nil}
	}
	for _, action := range []string{"enable", "disable"} {
		rules["POST /api/v1/system/nodes/:id/"+action] = auditRule{"system.node." + action, "node", nil}
	}
	return rules
}()

// auditForwarded records a mutation the gateway has committed. That commit is
// independent: an audit-write failure is logged, never turned into a failed
// response that invites duplicate work.
func auditForwarded(c *gin.Context, db *store.DB, uid int64, request, response []byte) {
	rule, ok := auditRules[c.Request.Method+" "+c.FullPath()]
	if !ok {
		return
	}
	var answer struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(response, &answer)
	id := cmp.Or(answer.Data.ID, c.Param("id"))
	details := map[string]any{"source": "console", "request_target": c.Param("id")}
	if rule.details != nil {
		target, extra, changed := rule.details(request, response)
		if !changed {
			return
		}
		id = cmp.Or(target, id)
		maps.Copy(details, extra)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(audit.Context(c)), 5*time.Second)
	defer cancel()
	if err := audit.Audit(ctx, db.Gorm, uid, rule.action, rule.targetType, id, details); err != nil {
		slog.Error("audit updater action failed", "action", rule.action, "target", id, "err", err)
	}
}

// updateSourceDetails records the stored source and what the gateway did to
// its secret (secret_change: set, cleared or unchanged; a moved source drops
// the secret unasked), never the secret. An answer that changed nothing is
// not audited.
func updateSourceDetails(_, response []byte) (string, map[string]any, bool) {
	var stored struct {
		Data struct {
			Kind         string `json:"kind"`
			Repository   string `json:"repository"`
			URL          string `json:"url"`
			Changed      bool   `json:"changed"`
			SecretChange string `json:"secret_change"`
		} `json:"data"`
	}
	_ = json.Unmarshal(response, &stored)
	d := stored.Data
	return "", map[string]any{"kind": d.Kind, "repository": d.Repository, "url": d.URL, "secret": d.SecretChange}, d.Changed
}

// applyDetails records a started update plan; an answer that still needs a
// confirmation started nothing.
func applyDetails(request, response []byte) (string, map[string]any, bool) {
	var asked struct {
		Line    string `json:"line"`
		Version string `json:"version"`
	}
	var answer struct {
		Data struct {
			Status string `json:"status"`
			PlanID string `json:"plan_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(request, &asked)
	_ = json.Unmarshal(response, &answer)
	if answer.Data.Status != "started" {
		return "", nil, false
	}
	return answer.Data.PlanID, map[string]any{"line": asked.Line, "version": asked.Version, "plan_id": answer.Data.PlanID}, true
}

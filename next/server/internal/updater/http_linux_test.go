//go:build linux

package updater

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

type bridgeAuth struct {
	testutil.StepUpPassed
	id int64
}

func (a bridgeAuth) VerifySession(context.Context, string) (int64, string, error) {
	return a.id, "", nil
}
func (a bridgeAuth) Can(context.Context, int64, string) (bool, error) { return true, nil }
func (a bridgeAuth) PermissionSet(context.Context, int64) (core.PermissionSet, error) {
	return core.PermissionSet{Superuser: true}, nil
}
func (a bridgeAuth) CanGrant(context.Context, int64, []string) error { return nil }

func TestBridgeAuditsOnlySuccessfulMutations(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	var uid int64
	if err := db.SQL.QueryRowContext(ctx, `INSERT INTO users(email,password_hash) VALUES('updater-audit@test','test') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "shell.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	shell := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "1" {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict"}}`))
			return
		}
		if r.Header.Get("X-Updater-Actor") != fmt.Sprint(uid) {
			t.Error("missing actor header")
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(201)
		switch {
		case r.URL.Path == "/system/update-source":
			_, _ = w.Write([]byte(`{"data":{"enabled":true,"kind":"github","repository":"owner/repo","url":"","has_secret":true,"changed":true,"secret_change":"set"}}`))
		case r.URL.Path == "/system/updates/apply" && strings.Contains(string(body), `"confirm":true`):
			_, _ = w.Write([]byte(`{"data":{"status":"started","plan_id":"applied-plan"}}`))
		case r.URL.Path == "/system/updates/apply":
			_, _ = w.Write([]byte(`{"data":{"status":"needs_confirmation","reasons":["single node"]}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"id":"new-plan"}}`))
		}
	})}
	go func() { _ = shell.Serve(listener) }()
	t.Cleanup(func() { _ = shell.Close() })
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	auth := bridgeAuth{id: uid}
	RegisterRoutes(httpapi.NewRouter(engine, auth, auth), socket, "", db)
	const secret = "github_pat_top-secret"
	cases := []struct{ method, path, body, action string }{
		{"POST", "/system/upgrades", `{}`, "system.upgrade.create"},
		{"POST", "/system/upgrades/old-plan/pause", `{}`, "system.upgrade.pause"},
		{"POST", "/system/upgrades/old-plan/resume", `{}`, "system.upgrade.resume"},
		{"POST", "/system/upgrades/old-plan/cancel", `{}`, "system.upgrade.cancel"},
		{"POST", "/system/upgrades/old-plan/rollback", `{}`, "system.upgrade.rollback"},
		{"POST", "/system/nodes/node-1/disable", `{}`, "system.node.disable"},
		{"POST", "/system/nodes/node-1/enable", `{}`, "system.node.enable"},
		{"PUT", "/system/offload", `{}`, "system.offload.update"},
		{"PUT", "/system/update-source", `{"enabled":true,"kind":"github","repository":"owner/repo","url":"","secret":"` + secret + `"}`, "system.update_source.update"},
		{"POST", "/system/updates/apply", `{"line":"core","version":"0.1.291"}`, ""},
		{"POST", "/system/updates/apply", `{"line":"core","version":"0.1.291","confirm":true}`, "system.update.apply"},
		{"POST", "/system/upgrades/preflight", `{}`, ""},
		{"GET", "/system/upgrades", ``, ""},
		{"POST", "/system/upgrades?fail=1", `{}`, ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "/api/v1"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer test")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, req)
		if response.Code != 201 && response.Code != 409 {
			t.Fatal(tc.path, response.Code, response.Body.String())
		}
		if tc.action != "" {
			var count int
			if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE action=$1 AND user_id=$2`, tc.action, uid).Scan(&count); err != nil || count != 1 {
				t.Fatal(tc.action, count, err)
			}
		}
	}
	var count int
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs`).Scan(&count); err != nil || count != 10 {
		t.Fatal(count, err)
	}
	var applied, source, leaked bool
	if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE action='system.update.apply' AND target_id='applied-plan' AND detail->>'line'='core' AND detail->>'version'='0.1.291' AND detail->>'plan_id'='applied-plan')`).Scan(&applied); err != nil || !applied {
		t.Fatal("missing apply audit details", err)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE action='system.update_source.update' AND detail->>'kind'='github' AND detail->>'repository'='owner/repo' AND detail->>'secret'='set')`).Scan(&source); err != nil || !source {
		t.Fatal("missing update source audit details", err)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE detail::text LIKE '%' || $1 || '%')`, secret).Scan(&leaked); err != nil || leaked {
		t.Fatal("secret reached the audit log", err)
	}
}

// A mutation the gateway is still committing when the console leaves is
// waited for, answered and audited.
func TestBridgeAuditsAMutationTheConsoleLeft(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	var uid int64
	if err := db.SQL.QueryRowContext(ctx, `INSERT INTO users(email,password_hash) VALUES('updater-left@test','test') RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "shell.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	shell := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		if r.Context().Err() != nil {
			t.Error("the forwarded request was cancelled with the console's")
		}
		_, _ = w.Write([]byte(`{"data":{"status":"started","plan_id":"late-plan"}}`))
	})}
	go func() { _ = shell.Serve(listener) }()
	t.Cleanup(func() { _ = shell.Close() })
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	auth := bridgeAuth{id: uid}
	RegisterRoutes(httpapi.NewRouter(engine, auth, auth), socket, "", db)
	console, leave := context.WithCancel(ctx)
	req := httptest.NewRequest("POST", "/api/v1/system/updates/apply", strings.NewReader(`{"line":"core","version":"0.1.291","confirm":true}`)).WithContext(console)
	req.Header.Set("Authorization", "Bearer test")
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, req)
		done <- res
	}()
	<-entered
	leave()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if res := <-done; res.Code != 200 {
		t.Fatalf("answer after the console left: %d %s", res.Code, res.Body.String())
	}
	var audited bool
	if err := db.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_logs WHERE action='system.update.apply' AND target_id='late-plan' AND user_id=$1)`, uid).Scan(&audited); err != nil || !audited {
		t.Fatal("the committed apply was not audited", err)
	}
}

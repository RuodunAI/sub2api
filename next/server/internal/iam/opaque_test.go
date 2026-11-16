package iam

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/cluster"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
)

// Opaque sessions (CONTRACTS §100): the session.issue switch, both kinds of
// token side by side, revocation at once, the refresh exchange, step-up
// rotation, the flows in Valkey and the outage.

// rootID is the bootstrap super admin.
func rootID(t *testing.T, s *Service) int64 {
	t.Helper()
	var id int64
	if err := userByEmail(s.db.Gorm, adminEmail, "id").Row().Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// issue switches session.issue as the super admin.
func issue(t *testing.T, s *Service, mode string) {
	t.Helper()
	if _, err := s.SetIssue(context.Background(), rootID(t, s), mode); err != nil {
		t.Fatalf("set %s: %v", mode, err)
	}
	if got := s.issueMode(context.Background()); got != mode {
		t.Fatalf("issuing %s after switching to %s", got, mode)
	}
}

// opaqueLogin signs in and checks that an opaque session came out.
func opaqueLogin(t *testing.T, s *Service, email, password string) *TokenPair {
	t.Helper()
	p := mustLogin(t, s, email, password)
	if !strings.HasPrefix(p.AccessToken, tokenstore.SessionPrefix) || p.RefreshToken != "" {
		t.Fatalf("login gave %+v", p)
	}
	return p
}

// sid is the login session of a live token.
func sid(t *testing.T, s *Service, token string) string {
	t.Helper()
	_, id, err := s.VerifySession(context.Background(), token)
	if err != nil || id == "" {
		t.Fatalf("session of %q: %q %v", token, id, err)
	}
	return id
}

func live(t *testing.T, s *Service, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if _, err := s.VerifyAccessToken(context.Background(), tok); err != nil {
			t.Fatalf("%.12s…: %v", tok, err)
		}
	}
}

func ended(t *testing.T, s *Service, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if _, err := s.VerifyAccessToken(context.Background(), tok); !isCode(err, "unauthenticated") {
			t.Fatalf("%.12s… still works: %v", tok, err)
		}
	}
}

func TestIssueSwitch(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	if st, err := s.IssueStatus(ctx); err != nil || st.Issue != "" || st.Effective != IssueJWT || st.Source != "default" ||
		st.IdleTTLSeconds != int64(e.cfg.SessionIdleTTL/time.Second) {
		t.Fatalf("default status %+v, %v", st, err)
	}
	jwtPair := mustLogin(t, s, adminEmail, adminPassword)
	if strings.HasPrefix(jwtPair.AccessToken, tokenstore.SessionPrefix) || jwtPair.RefreshToken == "" {
		t.Fatalf("default login gave %+v", jwtPair)
	}
	if _, err := s.SetIssue(ctx, root, "opaque"); !isCode(err, "invalid_argument") {
		t.Fatalf("unknown mode: %v", err)
	}
	admin, err := s.CreateUser(ctx, root, CreateUserInput{Email: "admin@example.com", Password: adminPassword, RoleKeys: []string{authz.RoleAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIssue(ctx, admin.ID, IssueOpaque); !isCode(err, "permission_denied") {
		t.Fatalf("an admin switched: %v", err)
	}

	// A second node follows config:changed.
	az2 := authz.New(authz.Deps{DB: s.db, Bus: e.bus, Shadow: authztest.Shadow()})
	if err := az2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	node2 := New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: az2, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	if node2.issueMode(ctx) != IssueJWT {
		t.Fatal("node 2 does not start on jwt")
	}
	st, err := s.SetIssue(ctx, root, IssueOpaque)
	if err != nil || st.Issue != IssueOpaque || st.Effective != IssueOpaque || st.Source != "setting" {
		t.Fatalf("set s2s: %+v, %v", st, err)
	}
	var audited int
	if err := s.db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'session.issue.set' AND user_id = ? AND target_id = 'session'
		AND detail->>'from' = '' AND detail->>'to' = 's2s'`, root).Row().Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit rows %d, %v", audited, err)
	}
	if node2.issueMode(ctx) != IssueOpaque {
		t.Fatal("node 2 missed the switch")
	}
	opaque := opaqueLogin(t, node2, adminEmail, adminPassword)
	if opaque.ExpiresIn != int64(e.cfg.SessionIdleTTL/time.Second) || opaque.User == nil || opaque.User.ID != root {
		t.Fatalf("opaque login %+v", opaque)
	}
	// Both kinds work on both nodes, whatever is issued.
	live(t, s, jwtPair.AccessToken, opaque.AccessToken)
	live(t, node2, jwtPair.AccessToken, opaque.AccessToken)
	issue(t, s, IssueJWT)
	live(t, s, jwtPair.AccessToken, opaque.AccessToken)

	// The environment pins a node whatever is stored.
	t.Setenv(IssueEnv, IssueOpaque)
	pinned := New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: az2, BcryptCost: bcrypt.MinCost})
	if st, err := pinned.IssueStatus(ctx); err != nil || st.Effective != IssueOpaque || st.Source != "env" || st.Issue != IssueJWT {
		t.Fatalf("pinned node %+v, %v", st, err)
	}
	// Without a session store (the server CLI) nothing opaque is issued.
	cli := New(Deps{DB: s.db, Config: e.cfg, Authz: az2})
	if cli.issuesOpaque(ctx) {
		t.Fatal("the CLI issues opaque sessions")
	}
	// and says so, though pinned to s2s.
	st, err = cli.IssueStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, IssueJWT, st.Effective)
	assert.Equal(t, "env", st.Source)
}

// A session.issue that is not a string keeps the mode a node issues: it is
// not read as the default (jwt).
func TestIssueKeepsItsModeOnABadSetting(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	issue(t, s, IssueOpaque)
	require.NoError(t, s.db.Gorm.Exec(`UPDATE settings SET value = '{"issue": 1}' WHERE key = 'session'`).Error)
	s.issueChanged([]byte(`{"key":"settings.session"}`))
	assert.Equal(t, IssueOpaque, s.issueMode(ctx))
	_, err := s.IssueStatus(ctx)
	assert.ErrorContains(t, err, "not a string")
}

// Switching takes a step-up proof (session.issue) besides settings:manage;
// reading does not.
func TestIssueRoutes(t *testing.T) {
	t.Setenv(IssueEnv, "")
	k := newKeyEnv(t)
	token := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	if code, out := k.call(token, "GET", "/settings/session", nil); code != 200 || out["data"].(map[string]any)["effective"] != IssueJWT {
		t.Fatalf("GET: %d %v", code, out)
	}
	body := map[string]any{"issue": IssueOpaque}
	if code, out := k.call(token, "PUT", "/settings/session", body); code != 403 || errCode(out) != "verification_required" {
		t.Fatalf("PUT without a proof: %d %v", code, out)
	}
	proof := k.proof(t, token, core.StepUpSessionIssue, "PUT", "/settings/session", map[string]any{"method": "password", "password": adminPassword})
	if code, out := k.callProof(token, proof, "PUT", "/settings/session", body); code != 200 || out["data"].(map[string]any)["effective"] != IssueOpaque {
		t.Fatalf("PUT: %d %v", code, out)
	}
}

// A refresh token presented after the switch is exchanged for an opaque
// session of the same id: the user stays signed in. Presented again, it
// ends that session too. A JWT issued before the switch works until it
// expires.
func TestRefreshExchangesForOpaqueSession(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	old, err := s.Login(withLoginMethod(ctx, loginMethodPassword), adminEmail, adminPassword, testIP)
	if err != nil {
		t.Fatal(err)
	}
	family := familyOf(t, s, old.AccessToken)
	issue(t, s, IssueOpaque)
	live(t, s, old.AccessToken)

	pair, err := s.Refresh(ctx, old.RefreshToken)
	if err != nil || !strings.HasPrefix(pair.AccessToken, tokenstore.SessionPrefix) || pair.RefreshToken != "" || pair.User == nil {
		t.Fatalf("exchange: %+v, %v", pair, err)
	}
	if got := sid(t, s, pair.AccessToken); got != family {
		t.Fatalf("exchanged session %q, want the family %q", got, family)
	}
	list, err := s.Sessions(ctx, pair.User.ID, family)
	if err != nil || len(list) != 1 || list[0].ID != family || !list[0].Current || list[0].Method != loginMethodPassword {
		t.Fatalf("sessions after the exchange %+v, %v", list, err)
	}
	// The old access token still works until it expires.
	live(t, s, old.AccessToken)
	e.cfg.AccessTokenTTL = -time.Minute
	expired, err := s.issueAccessToken(pair.User.ID, 0, family)
	if err != nil {
		t.Fatal(err)
	}
	ended(t, s, expired)

	// The refresh token is spent: presented again past the grace, it is a
	// replay.
	if err := s.db.Gorm.Exec(`UPDATE refresh_tokens SET replaced_at = replaced_at - interval '2 minutes',
		revoked_at = revoked_at - interval '2 minutes' WHERE family_id = ?`, family).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, old.RefreshToken); !isCode(err, "unauthenticated") {
		t.Fatalf("refresh token reused: %v", err)
	}
	ended(t, s, pair.AccessToken)
}

// Revocation ends an opaque session at once, and the sessions it keeps
// survive the token_version bump that stops the JWTs.
func TestOpaqueRevocation(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	jwtPair := mustLogin(t, s, adminEmail, adminPassword)
	issue(t, s, IssueOpaque)
	a := opaqueLogin(t, s, adminEmail, adminPassword)
	b := opaqueLogin(t, s, adminEmail, adminPassword)
	c := opaqueLogin(t, s, adminEmail, adminPassword)
	id := a.User.ID

	if err := s.RevokeSession(ctx, id, sid(t, s, a.AccessToken)); err != nil {
		t.Fatal(err)
	}
	ended(t, s, a.AccessToken)
	live(t, s, b.AccessToken, c.AccessToken)
	// No JWT of that session is valid: nothing else is touched, the JWT
	// session's access token included.
	live(t, s, jwtPair.AccessToken)
	bob, err := s.CreateUser(ctx, id, CreateUserInput{Email: "bob@example.com", Password: "password-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, bob.ID, sid(t, s, b.AccessToken)); !isCode(err, "not_found") {
		t.Fatalf("revoked another user's session: %v", err)
	}
	live(t, s, b.AccessToken)

	if err := s.RevokeOtherSessions(ctx, id, sid(t, s, b.AccessToken)); err != nil {
		t.Fatal(err)
	}
	ended(t, s, c.AccessToken)
	live(t, s, b.AccessToken)
	if list, err := s.Sessions(ctx, id, sid(t, s, b.AccessToken)); err != nil || len(list) != 1 || !list[0].Current {
		t.Fatalf("sessions %+v, %v", list, err)
	}

	// Changing the password keeps the caller's session only.
	d := opaqueLogin(t, s, adminEmail, adminPassword)
	if err := s.ChangePassword(ctx, id, sid(t, s, b.AccessToken), "another-password-1"); err != nil {
		t.Fatal(err)
	}
	ended(t, s, d.AccessToken)
	live(t, s, b.AccessToken)

	// Logging out without a refresh token ends the caller's opaque session.
	f := opaqueLogin(t, s, adminEmail, "another-password-1")
	if err := s.Logout(core.WithOpaqueSession(ctx, sid(t, s, f.AccessToken)), id, ""); err != nil {
		t.Fatal(err)
	}
	ended(t, s, f.AccessToken)
	live(t, s, b.AccessToken)

	// A token_version bump that did not go through Valkey (the server CLI)
	// still ends them, and they leave the list.
	if err := s.db.Gorm.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id = ?`, id).Error; err != nil {
		t.Fatal(err)
	}
	s.forgetStatus(id)
	ended(t, s, b.AccessToken)
	if list, err := s.sessions.List(ctx, id); err != nil || len(list) != 0 {
		t.Fatalf("left in Valkey: %+v, %v", list, err)
	}
}

// Every way an operator ends a user's sessions ends the opaque ones.
func TestOpaqueSessionsEndWithTheUser(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	issue(t, s, IssueOpaque)
	u, err := s.CreateUser(ctx, root, CreateUserInput{Email: "bob@example.com", Password: "password-1"})
	if err != nil {
		t.Fatal(err)
	}
	pw := "password-2"
	steps := []struct {
		name string
		run  func() error
	}{
		{"password reset", func() error {
			_, err := s.UpdateUser(withProof(t, s, ctx, root, core.StepUpUserPasswordReset), root, u.ID, UpdateUserInput{Password: &pw})
			return err
		}},
		{"security reset", func() error { return s.ResetSecurity(ctx, root, u.ID) }},
		{"disable", func() error {
			st := StatusDisabled
			_, err := s.UpdateUser(ctx, root, u.ID, UpdateUserInput{Status: &st})
			return err
		}},
	}
	password := "password-1"
	for _, step := range steps {
		p := opaqueLogin(t, s, u.Email, password)
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		ended(t, s, p.AccessToken)
		password = pw
	}
	active := StatusActive
	if _, err := s.UpdateUser(ctx, root, u.ID, UpdateUserInput{Status: &active}); err != nil {
		t.Fatal(err)
	}
	p := opaqueLogin(t, s, u.Email, pw)
	if err := s.DeleteUser(ctx, root, u.ID); err != nil {
		t.Fatal(err)
	}
	ended(t, s, p.AccessToken)

	// Resetting one's own factors keeps the session doing it.
	mine := opaqueLogin(t, s, adminEmail, adminPassword)
	other := opaqueLogin(t, s, adminEmail, adminPassword)
	if err := s.ResetSecurity(core.WithOpaqueSession(ctx, sid(t, s, mine.AccessToken)), root, root); err != nil {
		t.Fatal(err)
	}
	live(t, s, mine.AccessToken)
	ended(t, s, other.AccessToken)
}

// A session that verifies gets a new token of the same session: the proof,
// bound to the session, is spent with the new token; the old one works for
// the requests already sent with it.
func TestStepUpRotatesTheSession(t *testing.T) {
	t.Setenv(IssueEnv, "")
	k := newKeyEnv(t)
	issue(t, k.s, IssueOpaque)
	old := opaqueLogin(t, k.s, adminEmail, adminPassword).AccessToken
	path := "/users/" + strconv.FormatInt(k.userID, 10)
	code, out := k.call(old, "POST", "/me/verify", map[string]any{"scope": core.StepUpUserDelete, "context": "DELETE /api/v1" + path,
		"method": "password", "password": adminPassword})
	if code != 200 {
		t.Fatalf("verify: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	fresh, _ := data["access_token"].(string)
	if !strings.HasPrefix(fresh, tokenstore.SessionPrefix) || fresh == old {
		t.Fatalf("verify gave access token %q", fresh)
	}
	if sid(t, k.s, fresh) != sid(t, k.s, old) {
		t.Fatal("the rotation changed the session id")
	}
	if code, out := k.callProof(fresh, data["proof"].(string), "DELETE", path, nil); code != 204 {
		t.Fatalf("proof spent with the new token: %d %v", code, out)
	}
	k.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	ended(t, k.s, old)
	live(t, k.s, fresh)

	// A JWT session verifies as before, without a new token.
	issue(t, k.s, IssueJWT)
	jwtToken := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	code, out = k.call(jwtToken, "POST", "/me/verify", map[string]any{"scope": core.StepUpUserDelete, "context": "DELETE /api/v1/users/1",
		"method": "password", "password": adminPassword})
	if code != 200 || out["data"].(map[string]any)["access_token"] != nil {
		t.Fatalf("JWT verify: %d %v", code, out)
	}
}

// After the switch flows live in Valkey, taken once; a flow started before
// it, in auth_flows, still finishes.
func TestFlowsMoveToValkey(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	secret, _ := enableTwoFactor(t, s, id)
	before := mustLogin(t, s, adminEmail, adminPassword)
	issue(t, s, IssueOpaque)
	after := mustLogin(t, s, adminEmail, adminPassword)
	var rows int
	if err := s.db.Gorm.Raw(`SELECT count(*) FROM auth_flows WHERE purpose = ?`, flowLogin2FA).Row().Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("auth_flows rows %d, %v: the flow after the switch went to the table", rows, err)
	}
	for _, step := range []*TokenPair{before, after} {
		clearLastStep(t, s, id)
		pair, err := s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
		if err != nil || !strings.HasPrefix(pair.AccessToken, tokenstore.SessionPrefix) {
			t.Fatalf("second factor: %+v, %v", pair, err)
		}
		clearLastStep(t, s, id)
		if _, err := s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP); !isCode(err, "invalid_argument") || isBadSecondFactor(err) {
			t.Fatalf("flow reused: %v", err)
		}
	}
	// A flow is its owner's only.
	token, err := s.createFlow(ctx, s.db.Gorm, flowTOTPSetup, id, totpSetupPayload{Sealed: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var p totpSetupPayload
	if _, err := s.peekFlow(ctx, token, flowTOTPSetup, id+1, &p); err != errFlowExpired {
		t.Fatalf("another user's flow: %v", err)
	}
	if owner, err := s.takeFlow(ctx, s.db.Gorm, token, flowTOTPSetup, id, &p); err != nil || owner != id || p.Sealed != "x" {
		t.Fatalf("take: %d %+v %v", owner, p, err)
	}
}

// Step-up windows opened after the switch are Valkey windows.
func TestStepUpWindowsInValkey(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s := e.svc
	issue(t, s, IssueOpaque)
	ctx := core.WithSession(core.WithUserID(context.Background(), rootID(t, s)), "fam")
	ctx = core.WithStepUp(ctx, "", testOperation)
	core.MarkStepUpSpent(ctx, core.StepUpHostInstall, false)
	if err := s.OpenStepUpSession(ctx, core.StepUpHostInstall, "x/1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ContinueStepUpSession(ctx, core.StepUpHostInstall, "x/1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ContinueStepUpSession(ctx, core.StepUpHostInstall, "x/2"); !isCode(err, "verification_expired") {
		t.Fatalf("another window: %v", err)
	}
	var rows int
	if err := s.db.Gorm.Raw(`SELECT count(*) FROM auth_flows`).Row().Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("auth_flows rows %d, %v", rows, err)
	}
	e.redis.FastForward(core.StepUpSessionIdle + time.Second)
	if err := s.ContinueStepUpSession(ctx, core.StepUpHostInstall, "x/1"); !isCode(err, "verification_expired") {
		t.Fatalf("idle window: %v", err)
	}
}

// Valkey down: opaque sessions and logins fail with 503 (never 401, which
// signs the console out); JWTs, which do not read it, still work.
func TestValkeyDownIsAnOutage(t *testing.T) {
	t.Setenv(IssueEnv, "")
	k := newKeyEnv(t)
	jwtToken := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	issue(t, k.s, IssueOpaque)
	opaque := opaqueLogin(t, k.s, adminEmail, adminPassword).AccessToken
	k.redis.Close()
	if _, err := k.s.VerifyAccessToken(context.Background(), opaque); core.AsError(err).Status != http.StatusServiceUnavailable {
		t.Fatalf("verify: %v", err)
	}
	if code, out := k.call(opaque, "GET", "/me", nil); code != http.StatusServiceUnavailable || errCode(out) != "unavailable" {
		t.Fatalf("GET /me: %d %v", code, out)
	}
	if _, err := k.s.Login(context.Background(), adminEmail, adminPassword, testIP); core.AsError(err).Status != http.StatusServiceUnavailable {
		t.Fatalf("login: %v", err)
	}
	if code, out := k.call(jwtToken, "GET", "/me", nil); code != 200 {
		t.Fatalf("JWT GET /me: %d %v", code, out)
	}
}

// A passkey login issues an opaque session too, its ceremonies in Valkey.
func TestPasskeyLoginIssuesOpaqueSession(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	issue(t, s, IssueOpaque)
	r := consoleRequest(consoleOrigin, consoleHost)
	a := newAuthenticator(t, consoleOrigin, consoleHost)
	addPasskey(t, s, a, id)
	// A login whose transaction fails leaves no session behind.
	begin, err := s.BeginPasskeyLogin(ctx, r, testIP)
	require.NoError(t, err)
	restore := failAudit(t, s, "auth.superadmin_login", true)
	_, err = s.FinishPasskeyLogin(withLoginMethod(ctx, loginMethodPasskey), r, begin.FlowToken, a.get(t, begin), testIP)
	restore()
	require.Error(t, err)
	opaque, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, opaque)

	begin, err = s.BeginPasskeyLogin(ctx, r, testIP)
	if err != nil {
		t.Fatal(err)
	}
	credential := a.get(t, begin)
	pair, err := s.FinishPasskeyLogin(withLoginMethod(ctx, loginMethodPasskey), r, begin.FlowToken, credential, testIP)
	if err != nil || !strings.HasPrefix(pair.AccessToken, tokenstore.SessionPrefix) {
		t.Fatalf("passkey login: %+v, %v", pair, err)
	}
	if list, err := s.Sessions(ctx, id, ""); err != nil || len(list) != 1 || list[0].Method != loginMethodPasskey {
		t.Fatalf("sessions %+v, %v", list, err)
	}
	if _, err := s.FinishPasskeyLogin(ctx, r, begin.FlowToken, credential, testIP); !isCode(err, "invalid_argument") {
		t.Fatalf("replay: %v", err)
	}
	var rows int
	if err := s.db.Gorm.Raw(`SELECT count(*) FROM auth_flows`).Row().Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("auth_flows rows %d, %v", rows, err)
	}
}

// ------------------------------------------------------------ review fixes

// sessKey is the Valkey key of an opaque token.
func sessKey(token string) string { return "s2a:sess:" + tokenstore.Hash(token) }

// A bump that keeps the caller's session keeps the token a step-up rotation
// left in its grace too: a request still carrying it does not end the
// session. A token left behind that is stale ends alone; a second rotation
// ends the first token at once.
func TestKeptSessionKeepsItsRotatedToken(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	issue(t, s, IssueOpaque)
	t0 := opaqueLogin(t, s, adminEmail, adminPassword)
	id, uid := sid(t, s, t0.AccessToken), t0.User.ID
	t1, err := s.sessions.Rotate(ctx, uid, id)
	require.NoError(t, err)
	require.NoError(t, s.ChangePassword(ctx, uid, id, "another-password-1"))
	live(t, s, t0.AccessToken, t1)

	t2, err := s.sessions.Rotate(ctx, uid, id)
	require.NoError(t, err)
	ended(t, s, t0.AccessToken)
	e.redis.HSet(sessKey(t1), "tv", "0")
	ended(t, s, t1)
	live(t, s, t2)
}

// A refresh token exchanged moments ago and presented again (the console
// lost the answer) gets the session a new token instead of ending it.
func TestRefreshRetriedWithinTheGrace(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	old := mustLogin(t, s, adminEmail, adminPassword)
	issue(t, s, IssueOpaque)
	first, err := s.Refresh(ctx, old.RefreshToken)
	require.NoError(t, err)
	second, err := s.Refresh(ctx, old.RefreshToken)
	require.NoError(t, err)
	require.NotNil(t, second.User)
	assert.NotEqual(t, first.AccessToken, second.AccessToken)
	assert.Empty(t, second.RefreshToken)
	assert.Equal(t, sid(t, s, first.AccessToken), sid(t, s, second.AccessToken))
	e.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	ended(t, s, first.AccessToken)
	live(t, s, second.AccessToken)
	list, err := s.Sessions(ctx, second.User.ID, "")
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// Logging out with an opaque session ends that session only, even when it
// is gone already; a JWT caller without a refresh token still ends them all.
func TestOpaqueLogoutEndsOnlyItself(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	issue(t, s, IssueOpaque)
	a := opaqueLogin(t, s, adminEmail, adminPassword)
	b := opaqueLogin(t, s, adminEmail, adminPassword)
	idA, uid := sid(t, s, a.AccessToken), a.User.ID
	require.NoError(t, s.sessions.Revoke(ctx, uid, tokenstore.Selection{Only: idA}, tokenstore.Bump{}))
	require.NoError(t, s.Logout(core.WithOpaqueSession(ctx, idA), uid, ""))
	live(t, s, b.AccessToken)
	require.NoError(t, s.Logout(core.WithSession(ctx, "some-family"), uid, ""))
	ended(t, s, b.AccessToken)
}

// The JWT access token of a family exchanged for an opaque session verifies
// without rotating that session; revoking the session then ends the JWT too
// (it may still be valid, so revoking bumps the version).
func TestOldJWTOfExchangedFamily(t *testing.T) {
	t.Setenv(IssueEnv, "")
	k := newKeyEnv(t)
	old := mustLogin(t, k.s, adminEmail, adminPassword)
	issue(t, k.s, IssueOpaque)
	pair, err := k.s.Refresh(context.Background(), old.RefreshToken)
	require.NoError(t, err)
	code, out := k.call(old.AccessToken, "POST", "/me/verify", map[string]any{"scope": core.StepUpUserDelete,
		"context": "DELETE /api/v1/users/1", "method": "password", "password": adminPassword})
	require.Equal(t, 200, code, out)
	assert.Nil(t, out["data"].(map[string]any)["access_token"])
	k.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	live(t, k.s, pair.AccessToken)

	require.NoError(t, k.s.RevokeSession(context.Background(), pair.User.ID, sid(t, k.s, pair.AccessToken)))
	ended(t, k.s, pair.AccessToken, old.AccessToken)
}

// Revoking one opaque session no JWT of which is valid touches that session
// alone: no token_version bump.
func TestRevokingAnOpaqueSessionTouchesItAlone(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	jwtPair := mustLogin(t, s, adminEmail, adminPassword)
	issue(t, s, IssueOpaque)
	a := opaqueLogin(t, s, adminEmail, adminPassword)
	b := opaqueLogin(t, s, adminEmail, adminPassword)
	var before, after int
	require.NoError(t, userTokenVersion(s.db.Gorm, a.User.ID).Row().Scan(&before))
	require.NoError(t, s.RevokeSession(ctx, a.User.ID, sid(t, s, a.AccessToken)))
	require.NoError(t, userTokenVersion(s.db.Gorm, a.User.ID).Row().Scan(&after))
	assert.Equal(t, before, after)
	ended(t, s, a.AccessToken)
	live(t, s, b.AccessToken, jwtPair.AccessToken)
}

// A stamp a rolled-back bump left on a kept session (one ahead of the row)
// does not survive a later bump that ends every session without Valkey
// (the server CLI).
func TestRolledBackStampDoesNotSurviveABump(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	issue(t, s, IssueOpaque)
	a := opaqueLogin(t, s, adminEmail, adminPassword)
	uid := a.User.ID
	var tv int
	require.NoError(t, userTokenVersion(s.db.Gorm, uid).Row().Scan(&tv))
	// What a keep bump stamps before its commit, which then fails.
	require.NoError(t, s.sessions.Revoke(ctx, uid, tokenstore.Selection{Only: "none"}, tokenstore.Bump{From: tv, To: tv + 1}))
	live(t, s, a.AccessToken)

	cli := New(Deps{DB: s.db, Config: e.cfg, Authz: e.authz, BcryptCost: bcrypt.MinCost})
	require.NoError(t, cli.ChangePassword(ctx, uid, "", "another-password-1"))
	s.forgetStatus(uid)
	ended(t, s, a.AccessToken)
}

// failAudit makes PostgreSQL fail the statements or the commit of the
// transactions that audit action: what fails a change after its checks.
func failAudit(t *testing.T, s *Service, action string, atCommit bool) func() {
	t.Helper()
	trigger := `CREATE TRIGGER fail_audit BEFORE INSERT ON audit_logs FOR EACH ROW
		WHEN (NEW.action = '` + action + `') EXECUTE FUNCTION fail_audit()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER fail_audit AFTER INSERT ON audit_logs DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (NEW.action = '` + action + `') EXECUTE FUNCTION fail_audit()`
	}
	require.NoError(t, s.db.Gorm.Exec(`CREATE OR REPLACE FUNCTION fail_audit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'the change fails here'; END $$`).Error)
	require.NoError(t, s.db.Gorm.Exec(trigger).Error)
	return func() { require.NoError(t, s.db.Gorm.Exec(`DROP TRIGGER fail_audit ON audit_logs`).Error) }
}

// A login whose transaction fails leaves no opaque session behind, and a
// second step that fails before its end leaves the flow to try again.
func TestFailedLoginLeavesNothingBehind(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	issue(t, s, IssueOpaque)

	restore := failAudit(t, s, "auth.superadmin_login", true)
	_, err := s.Login(ctx, adminEmail, adminPassword, testIP)
	require.Error(t, err)
	restore()
	list, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, list)

	secret, _ := enableTwoFactor(t, s, id)
	step := mustLogin(t, s, adminEmail, adminPassword)
	require.True(t, step.TwoFactorRequired)
	restore = failAudit(t, s, "auth.superadmin_login", false)
	clearLastStep(t, s, id)
	_, err = s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.ErrorContains(t, err, "the change fails here")
	restore()
	clearLastStep(t, s, id)
	pair, err := s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(pair.AccessToken, tokenstore.SessionPrefix))

	// The flow taken by another request meanwhile: the session issued
	// before the take is ended.
	step = mustLogin(t, s, adminEmail, adminPassword)
	_, err = s.flows.TakeFlow(ctx, flowLogin2FA, step.FlowToken)
	require.NoError(t, err)
	clearLastStep(t, s, id)
	before, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	_, err = s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.Error(t, err)
	after, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Len(t, after, len(before))
}

// rotateFails is a session store whose rotations fail as Valkey down does.
type rotateFails struct{ tokenstore.SessionStore }

func (rotateFails) Rotate(context.Context, int64, string) (string, error) {
	return "", tokenstore.ErrUnavailable
}

// Valkey down, issuing JWTs: a JWT session verifies, spends the proof,
// lists and ends its sessions, and an operator disables a user; only what
// Valkey holds fails. A rotation that fails leaves no audit and no proof.
func TestJWTSessionsDoNotNeedValkey(t *testing.T) {
	t.Setenv(IssueEnv, "")
	k := newKeyEnv(t)
	ctx := context.Background()
	jwtPair := mustLogin(t, k.s, adminEmail, adminPassword)
	other := mustLogin(t, k.s, adminEmail, adminPassword)
	victim := mustLogin(t, k.s, "key-user@example.com", adminPassword)
	k.redis.Close()

	path := "/users/" + strconv.FormatInt(k.userID, 10)
	proof := k.proof(t, jwtPair.AccessToken, core.StepUpUserDelete, "DELETE", path, map[string]any{"method": "password", "password": adminPassword})
	code, out := k.call(jwtPair.AccessToken, "GET", "/me/sessions", nil)
	require.Equal(t, 200, code, out)
	code, out = k.call(jwtPair.AccessToken, "POST", "/me/sessions/revoke-others", nil)
	require.Equal(t, 204, code, out)
	ended(t, k.s, other.AccessToken)
	jwtPair, err := k.s.Refresh(ctx, jwtPair.RefreshToken) // the kept JWT session renews
	require.NoError(t, err)
	disabled := StatusDisabled
	_, err = k.s.UpdateUser(ctx, rootID(t, k.s), k.userID, UpdateUserInput{Status: &disabled})
	require.NoError(t, err)
	ended(t, k.s, victim.AccessToken)
	code, out = k.callProof(jwtPair.AccessToken, proof, "DELETE", path, nil)
	require.Equal(t, 204, code, out)
	require.NoError(t, k.s.Logout(core.WithSession(ctx, familyOf(t, k.s, jwtPair.AccessToken)), rootID(t, k.s), jwtPair.RefreshToken))

	// An opaque session whose rotation fails: the verification leaves
	// nothing behind.
	mr := miniredis.RunT(t)
	s := New(Deps{DB: k.s.db, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}), Config: k.cfg, Authz: k.authz, BcryptCost: bcrypt.MinCost})
	s.sessions = rotateFails{s.sessions}
	issue(t, s, IssueOpaque)
	p := opaqueLogin(t, s, adminEmail, adminPassword)
	var audits int
	require.NoError(t, s.db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'user.security_verify'`).Row().Scan(&audits))
	_, err = s.VerifyStepUp(core.WithOpaqueSession(ctx, sid(t, s, p.AccessToken)), consoleRequest(consoleOrigin, consoleHost), p.User.ID,
		VerifyInput{Scope: core.StepUpUserDelete, Context: "DELETE /api/v1/users/1", Method: methodPassword, Password: adminPassword}, testIP)
	assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status)
	var after int
	require.NoError(t, s.db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'user.security_verify'`).Row().Scan(&after))
	assert.Equal(t, audits, after)
	for _, key := range mr.Keys() {
		assert.False(t, strings.HasPrefix(key, "s2a:flow:"+flowStepUpProof), key)
	}
}

// fakeNodes is the live nodes of a cluster.
type fakeNodes []core.NodeStatus

func (f fakeNodes) LiveNodes(context.Context) ([]core.NodeStatus, error) { return f, nil }

// Switching to s2s waits for every live node to accept opaque sessions;
// switching back does not.
func TestIssueWaitsForEveryNode(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	s.nodes = fakeNodes{
		{NodeID: "new", BootID: "b1", HostVersion: "0.2.0", Capabilities: []string{core.CapabilityOpaqueSessions}},
		{NodeID: "old", BootID: "b2", HostVersion: "0.1.0"},
	}
	st, err := s.IssueStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, []BehindNode{{NodeID: "old", BootID: "b2", HostVersion: "0.1.0"}}, st.Behind)
	_, err = s.SetIssue(ctx, root, IssueOpaque)
	assert.True(t, isCode(err, "conflict"), "%v", err)
	assert.Equal(t, []BehindNode{{NodeID: "old", BootID: "b2", HostVersion: "0.1.0"}}, core.AsError(err).Details["nodes"])
	assert.Equal(t, IssueJWT, s.issueMode(ctx))
	_, err = s.SetIssue(ctx, root, IssueJWT)
	require.NoError(t, err)

	s.nodes = fakeNodes{{NodeID: "new", BootID: "b1", Capabilities: []string{core.CapabilityOpaqueSessions}}}
	st, err = s.SetIssue(ctx, root, IssueOpaque)
	require.NoError(t, err)
	assert.Equal(t, IssueOpaque, st.Effective)
	assert.Empty(t, st.Behind)

	// The registry unreadable: no switch to s2s (503), the way back still
	// works.
	down := miniredis.RunT(t)
	addr := down.Addr()
	down.Close()
	s.nodes = cluster.NewRegistry(redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1}), nil, cluster.RegistryOptions{})
	_, err = s.SetIssue(ctx, root, IssueJWT)
	require.NoError(t, err)
	_, err = s.SetIssue(ctx, root, IssueOpaque)
	assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status, "%v", err)
	st, err = s.IssueStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, IssueJWT, st.Effective)
	assert.Nil(t, st.Behind)
}

// ------------------------------------------------------------ second review fixes

// endWithoutValkey ends every session of uid the way the server CLI does: a
// token_version bump that never reaches Valkey, which keeps the opaque
// sessions (as when the cleanup after the commit fails).
func endWithoutValkey(t *testing.T, e *env, uid int64, password string) {
	t.Helper()
	cli := New(Deps{DB: e.svc.db, Config: e.cfg, Authz: e.authz, BcryptCost: bcrypt.MinCost})
	require.NoError(t, cli.ChangePassword(context.Background(), uid, "", password))
	e.svc.forgetStatus(uid)
}

// outage fails the Valkey commands of the session stores it is put on
// while it is down, as Valkey not answering does (a go-redis hook: no
// server restart to wait for).
type outage struct{ down atomic.Bool }

// on gives s a session store on e's Valkey that fails while o is down. A
// token a rotation leaves behind lasts the test.
func (o *outage) on(e *env, s *Service) {
	rdb := redis.NewClient(&redis.Options{Addr: e.redis.Addr()})
	rdb.AddHook(o)
	s.sessions = tokenstore.NewValkey(rdb, tokenstore.Options{RotateGrace: time.Hour})
}

// during runs fn with Valkey down.
func (o *outage) during(fn func() error) error {
	o.down.Store(true)
	defer o.down.Store(false)
	return fn()
}

func (o *outage) DialHook(next redis.DialHook) redis.DialHook { return next }

func (o *outage) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if o.down.Load() {
			cmd.SetErr(errValkeyDown)
			return errValkeyDown
		}
		return next(ctx, cmd)
	}
}

func (o *outage) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if o.down.Load() {
			return errValkeyDown
		}
		return next(ctx, cmds)
	}
}

var errValkeyDown = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}

// A session that a bump ended, still in Valkey, stays ended through a later
// bump that keeps sessions: neither a JWT logout with its refresh token nor
// revoking a family just exchanged restamps it back to life.
func TestEndedSessionStaysEnded(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	keeps := []struct {
		name string
		run  func(password string) error
	}{
		{"JWT logout", func(password string) error {
			issue(t, s, IssueJWT)
			p := mustLogin(t, s, adminEmail, password)
			return s.Logout(ctx, uid, p.RefreshToken)
		}},
		{"revoking an exchanged family", func(password string) error {
			issue(t, s, IssueJWT)
			p := mustLogin(t, s, adminEmail, password)
			issue(t, s, IssueOpaque)
			x, err := s.Refresh(ctx, p.RefreshToken)
			require.NoError(t, err)
			return s.RevokeSession(ctx, uid, sid(t, s, x.AccessToken))
		}},
	}
	password := adminPassword
	for i, keep := range keeps {
		issue(t, s, IssueOpaque)
		z := opaqueLogin(t, s, adminEmail, password)
		password = "another-password-" + strconv.Itoa(i)
		endWithoutValkey(t, e, uid, password)
		require.NoError(t, keep.run(password), keep.name)
		s.forgetStatus(uid)
		ended(t, s, z.AccessToken)
	}
}

// Valkey down, issuing JWTs: revoking a JWT family whose access tokens have
// all expired goes through (a bump stands in for Valkey), and so does a
// replayed refresh token, whose bump reaches the other nodes at once.
func TestRevokingAnIdleFamilyWithValkeyDown(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	a := mustLogin(t, s, adminEmail, adminPassword)
	b := mustLogin(t, s, adminEmail, adminPassword)
	family := familyOf(t, s, b.AccessToken)
	require.NoError(t, s.db.Gorm.Exec(`UPDATE refresh_tokens SET created_at = now() - interval '3 hours' WHERE family_id = ?`, family).Error)
	e.redis.Close()
	require.NoError(t, s.RevokeSession(ctx, a.User.ID, family))
	a, err := s.Refresh(ctx, a.RefreshToken)
	require.NoError(t, err)
	other := New(Deps{DB: s.db, Config: e.cfg, Authz: e.authz, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	live(t, other, a.AccessToken)
	_, err = s.Refresh(ctx, b.RefreshToken)
	assert.True(t, isCode(err, "unauthenticated"), "%v", err)
	ended(t, other, a.AccessToken)
}

// Cleaning up after ending every session leaves the sessions that signed
// in since the commit.
func TestCleanupKeepsLaterLogins(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	issue(t, s, IssueOpaque)
	old := opaqueLogin(t, s, adminEmail, adminPassword)
	endWithoutValkey(t, e, uid, "another-password-1")
	fresh := opaqueLogin(t, s, adminEmail, "another-password-1")
	s.sessionsEnded(ctx, uid)
	live(t, s, fresh.AccessToken)
	assert.False(t, e.redis.Exists(sessKey(old.AccessToken)))
}

// A bump that keeps a session but cannot restamp (Valkey down) also ends a
// session that a rolled-back bump stamped one ahead.
func TestKeepingBumpWithValkeyDownEndsAheadStamps(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	issue(t, s, IssueOpaque)
	a := opaqueLogin(t, s, adminEmail, adminPassword)
	b := opaqueLogin(t, s, adminEmail, adminPassword)
	keep := sid(t, s, b.AccessToken)
	var tv int
	require.NoError(t, userTokenVersion(s.db.Gorm, uid).Row().Scan(&tv))
	e.redis.HSet(sessKey(a.AccessToken), "tv", strconv.Itoa(tv+1))
	var valkey outage
	valkey.on(e, s)
	require.NoError(t, valkey.during(func() error { return s.RevokeOtherSessions(ctx, uid, keep) }))
	s.forgetStatus(uid)
	ended(t, s, a.AccessToken)
}

// The sessions list leaves out the opaque sessions a bump ended that are
// still in Valkey, and revoking one finds no such session.
func TestSessionsLeaveOutEndedOnes(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	issue(t, s, IssueOpaque)
	old := sid(t, s, opaqueLogin(t, s, adminEmail, adminPassword).AccessToken)
	endWithoutValkey(t, e, uid, "another-password-1")
	fresh := opaqueLogin(t, s, adminEmail, "another-password-1")
	list, err := s.Sessions(ctx, uid, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, sid(t, s, fresh.AccessToken), list[0].ID)
	err = s.RevokeSession(ctx, uid, old)
	assert.True(t, isCode(err, "not_found"), "revoking an ended session: %v", err)
}

// ------------------------------------------------------------ third review fixes

// A bump that keeps the other sessions keeps them for a user still at
// token_version 0 too: after a JWT logout with its refresh token, or
// revoking a JWT family, the user's opaque sessions stay signed in.
func TestKeepingBumpFromVersionZero(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	keeps := []struct {
		name string
		run  func(uid int64, jwtPair *TokenPair) error
	}{
		{"JWT logout", func(uid int64, p *TokenPair) error { return s.Logout(ctx, uid, p.RefreshToken) }},
		{"revoking a JWT family", func(uid int64, p *TokenPair) error {
			return s.RevokeSession(ctx, uid, familyOf(t, s, p.AccessToken))
		}},
	}
	for i, keep := range keeps {
		email := "zero-" + strconv.Itoa(i) + "@example.com"
		u, err := s.CreateUser(ctx, root, CreateUserInput{Email: email, Password: adminPassword})
		require.NoError(t, err)
		issue(t, s, IssueJWT)
		j := mustLogin(t, s, email, adminPassword)
		issue(t, s, IssueOpaque)
		z := opaqueLogin(t, s, email, adminPassword)
		var tv int
		require.NoError(t, userTokenVersion(s.db.Gorm, u.ID).Row().Scan(&tv))
		require.Zero(t, tv)
		require.NoError(t, keep.run(u.ID, j), keep.name)
		live(t, s, z.AccessToken)
	}
}

// keepEnded is a session store whose Drop does nothing: a node that finds
// a token ended without removing it, so a later change still has the
// chance to revive it, which it must not.
type keepEnded struct{ tokenstore.SessionStore }

func (keepEnded) Drop(context.Context, string, tokenstore.Session) error { return nil }

// walkNodes are the nodes the walks run on: s issues opaque sessions,
// jwtNode JWTs, both verify either kind and share a bus; observer checks
// ended tokens without removing them; cli is the server CLI (no Valkey, no
// bus). valkey fails the session stores of s and jwtNode on demand.
type walkNodes struct {
	t                         *testing.T
	ctx                       context.Context
	s, jwtNode, observer, cli *Service
	valkey                    *outage
	rdb                       redis.Cmdable
}

// sessionWalk is one user's sessions through a walk of changes, and what
// the walk expects of every opaque token issued: one is live unless a
// committed change ended its session, or a second rotation ended it.
type sessionWalk struct {
	*walkNodes
	rng   *rand.Rand
	name  string
	uid   int64
	email string
	trail []string

	tokens   []walkToken
	sids     []string          // the opaque sessions, in the order issued
	current  map[string]string // session -> its token
	prev     map[string]string // session -> the token its last rotation left
	ended    map[string]bool   // sessions a committed change ended
	rotated  map[string]bool   // tokens a second rotation ended
	families []*walkFamily
	late     *lateRevoke
}

type walkToken struct{ value, sid string }

// walkFamily is a JWT login session: revoked once its refresh tokens are,
// exchanged once traded for an opaque session of its id.
type walkFamily struct {
	id, refresh        string
	revoked, exchanged bool
}

// lateRevoke is a revocation whose caller gave up on Valkey and bumped
// past it, which Valkey may still run.
type lateRevoke struct {
	keep string
	from int
	at   time.Time
}

// walkSteps are the changes a walk picks from, by weight.
var walkSteps = []struct {
	name   string
	weight int
	run    func(*sessionWalk)
}{
	{"login", 3, (*sessionWalk).login},
	{"rotate", 2, (*sessionWalk).rotate},
	{"JWT login", 2, (*sessionWalk).jwtLogin},
	{"exchange", 2, (*sessionWalk).exchange},
	{"grace retry", 1, (*sessionWalk).graceRetry},
	{"replay", 1, func(w *sessionWalk) { w.replay(false) }},
	{"replay, Valkey down", 1, func(w *sessionWalk) { w.replay(true) }},
	{"revoke one", 1, func(w *sessionWalk) { w.revokeOne(false) }},
	{"revoke one, Valkey down", 1, func(w *sessionWalk) { w.revokeOne(true) }},
	{"opaque logout", 1, func(w *sessionWalk) { w.opaqueLogout(false) }},
	{"opaque logout, Valkey down", 1, func(w *sessionWalk) { w.opaqueLogout(true) }},
	{"JWT logout", 1, func(w *sessionWalk) { w.jwtLogout(false) }},
	{"JWT logout, Valkey down", 1, func(w *sessionWalk) { w.jwtLogout(true) }},
	{"end all", 1, func(w *sessionWalk) { w.endAll(false) }},
	{"end all, cleanup fails", 1, func(w *sessionWalk) { w.endAll(true) }},
	{"revoke others", 1, func(w *sessionWalk) { w.revokeOthers(false) }},
	{"revoke others, Valkey down", 1, func(w *sessionWalk) { w.revokeOthers(true) }},
	{"revoke others, rolled back", 1, (*sessionWalk).revokeOthersRolledBack},
	{"late revoke", 1, (*sessionWalk).runLate},
	{"admin password reset", 1, (*sessionWalk).adminPasswordReset},
	{"admin disable", 1, (*sessionWalk).adminDisable},
	{"admin security reset", 1, (*sessionWalk).adminSecurityReset},
	{"CLI bump", 1, (*sessionWalk).cliBump},
}

// Whatever the order of the changes a user's sessions go through (logins,
// rotations, refresh tokens exchanged, retried within the grace and
// replayed, single and bulk revocations, logouts, operators' changes, the
// server CLI, rolled-back restamps), with Valkey failing meanwhile and a
// revocation Valkey runs late: a session stays ended once a committed
// change ended it, every token of it, and a session no change ended stays
// signed in, on both nodes. Each step is checked at once, an ended token
// without removing it (a later change must not revive it), and every
// token again at the end of the walk on both nodes. The walks are fixed
// pseudo-random sequences, from a user at token_version 0 and from one
// bumped; a failure names the one that broke it.
func TestEndedSessionsStayEndedInAnyOrder(t *testing.T) {
	t.Setenv(IssueEnv, IssueOpaque)
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	t.Setenv(IssueEnv, IssueJWT)
	node := func() *Service {
		return New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: e.authz, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	}
	nodes := &walkNodes{t: t, ctx: ctx, s: s, jwtNode: node(), observer: node(), valkey: &outage{},
		cli: New(Deps{DB: s.db, Config: e.cfg, Authz: e.authz, BcryptCost: bcrypt.MinCost}),
		rdb: redis.NewClient(&redis.Options{Addr: e.redis.Addr()})}
	nodes.valkey.on(e, s)
	nodes.valkey.on(e, nodes.jwtNode)
	nodes.observer.sessions = keepEnded{nodes.observer.sessions}
	for start, bumped := range []bool{false, true} {
		rng := rand.New(rand.NewPCG(100, uint64(start)))
		for n := range 30 {
			email := fmt.Sprintf("walk-%d-%d@example.com", start, n)
			u, err := s.CreateUser(ctx, root, CreateUserInput{Email: email, Password: adminPassword})
			require.NoError(t, err)
			if bumped {
				require.NoError(t, s.ResetSecurity(ctx, 0, u.ID))
			}
			w := &sessionWalk{walkNodes: nodes, rng: rng, name: fmt.Sprintf("bumped %v, walk %d", bumped, n), uid: u.ID, email: email,
				current: map[string]string{}, prev: map[string]string{}, ended: map[string]bool{}, rotated: map[string]bool{}}
			w.walk(12)
		}
	}
}

// walk runs steps changes picked by weight, checking every token after
// each, and once more at the end on both nodes.
func (w *sessionWalk) walk(steps int) {
	total := 0
	for _, st := range walkSteps {
		total += st.weight
	}
	for range steps {
		pick := w.rng.IntN(total)
		for _, st := range walkSteps {
			if pick -= st.weight; pick < 0 {
				w.trail = append(w.trail, st.name)
				st.run(w)
				break
			}
		}
		w.check(w.observer)
	}
	w.check(w.jwtNode)
	w.check(w.s)
}

// check verifies every token as the walk expects it: a live one on both
// nodes, an ended one on ended (it removes the ended tokens unless it is
// the observer).
func (w *sessionWalk) check(ended *Service) {
	w.t.Helper()
	for _, tk := range w.tokens {
		if !w.ended[tk.sid] && !w.rotated[tk.value] {
			for _, n := range []*Service{w.s, w.jwtNode} {
				_, err := n.VerifyAccessToken(w.ctx, tk.value)
				require.NoError(w.t, err, "%s: a token of a session no change ended is rejected", w.where())
			}
			continue
		}
		_, err := ended.VerifyAccessToken(w.ctx, tk.value)
		require.True(w.t, isCode(err, "unauthenticated"), "%s: a token of an ended session works: %v", w.where(), err)
	}
}

func (w *sessionWalk) where() string { return fmt.Sprintf("%s %q", w.name, w.trail) }

func (w *sessionWalk) ok(err error) {
	w.t.Helper()
	require.NoError(w.t, err, w.where())
}

// during runs fn, with Valkey down when down.
func (w *sessionWalk) during(down bool, fn func() error) error {
	if down {
		return w.valkey.during(fn)
	}
	return fn()
}

// issued records a token of the opaque session id: a rotation when the
// session has one already, which ends the token the last one left.
func (w *sessionWalk) issued(id, token string) {
	w.t.Helper()
	if cur, ok := w.current[id]; ok {
		if prev := w.prev[id]; prev != "" {
			w.rotated[prev] = true
		}
		w.prev[id] = cur
	} else {
		w.sids = append(w.sids, id)
	}
	w.current[id] = token
	w.tokens = append(w.tokens, walkToken{token, id})
}

// sessionOf is the opaque session of a live token.
func (w *sessionWalk) sessionOf(token string) string {
	w.t.Helper()
	_, id, err := w.s.VerifySession(w.ctx, token)
	w.ok(err)
	return id
}

// endSessions records that every opaque session but keep ended.
func (w *sessionWalk) endSessions(keep string) {
	for _, id := range w.sids {
		if id != keep {
			w.ended[id] = true
		}
	}
}

// revokeFamilies records that every family but keep was revoked.
func (w *sessionWalk) revokeFamilies(keep string) {
	for _, f := range w.families {
		if f.id != keep {
			f.revoked = true
		}
	}
}

// endedEverything records a change that ended every session.
func (w *sessionWalk) endedEverything() {
	w.endSessions("")
	w.revokeFamilies("")
}

// ended1 records that the session or family id ended.
func (w *sessionWalk) ended1(id string) {
	w.ended[id] = true
	if f := w.family(id); f != nil {
		f.revoked = true
	}
}

func (w *sessionWalk) family(id string) *walkFamily {
	for _, f := range w.families {
		if f.id == id {
			return f
		}
	}
	return nil
}

// pickSession is an opaque session issued so far, "" before any.
func (w *sessionWalk) pickSession() string {
	if len(w.sids) == 0 {
		return ""
	}
	return w.sids[w.rng.IntN(len(w.sids))]
}

// pickID is an opaque session or a family, "" before any.
func (w *sessionWalk) pickID() string {
	n := len(w.sids) + len(w.families)
	if n == 0 {
		return ""
	}
	i := w.rng.IntN(n)
	if i < len(w.sids) {
		return w.sids[i]
	}
	return w.families[i-len(w.sids)].id
}

// pickFamily is a family that ok accepts, nil when none does.
func (w *sessionWalk) pickFamily(ok func(*walkFamily) bool) *walkFamily {
	var fs []*walkFamily
	for _, f := range w.families {
		if ok(f) {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		return nil
	}
	return fs[w.rng.IntN(len(fs))]
}

func (w *sessionWalk) login() {
	p, err := w.s.Login(w.ctx, w.email, adminPassword, testIP)
	w.ok(err)
	w.issued(w.sessionOf(p.AccessToken), p.AccessToken)
}

// rotate gives a session a new token, as a step-up does; one Valkey no
// longer has is not found.
func (w *sessionWalk) rotate() {
	id := w.pickSession()
	if id == "" {
		return
	}
	token, err := w.s.sessions.Rotate(w.ctx, w.uid, id)
	if w.ended[id] && errors.Is(err, tokenstore.ErrNotFound) {
		return
	}
	w.ok(err)
	w.issued(id, token)
}

func (w *sessionWalk) jwtLogin() {
	p, err := w.jwtNode.Login(w.ctx, w.email, adminPassword, testIP)
	w.ok(err)
	w.families = append(w.families, &walkFamily{id: familyOf(w.t, w.jwtNode, p.AccessToken), refresh: p.RefreshToken})
}

// exchange trades a live family's refresh token for an opaque session of
// the family's id.
func (w *sessionWalk) exchange() {
	f := w.pickFamily(func(f *walkFamily) bool { return !f.revoked })
	if f == nil {
		return
	}
	p, err := w.s.Refresh(w.ctx, f.refresh)
	w.ok(err)
	require.Equal(w.t, f.id, w.sessionOf(p.AccessToken), w.where())
	f.revoked, f.exchanged = true, true
	w.issued(f.id, p.AccessToken)
}

// graceRetry presents an exchanged refresh token again within the grace:
// its session gets another token. An ended session may be gone, which
// makes it a replay.
func (w *sessionWalk) graceRetry() {
	f := w.pickFamily(func(f *walkFamily) bool { return f.exchanged })
	if f == nil {
		return
	}
	p, err := w.s.Refresh(w.ctx, f.refresh)
	if w.ended[f.id] && isCode(err, "unauthenticated") {
		return
	}
	w.ok(err)
	w.issued(f.id, p.AccessToken)
}

// replay presents a spent or revoked refresh token past the grace: its
// family's opaque session ends, and with Valkey down every one (a bump
// stands in).
func (w *sessionWalk) replay(down bool) {
	f := w.pickFamily(func(f *walkFamily) bool { return f.revoked })
	if f == nil {
		return
	}
	w.ok(w.s.db.Gorm.Exec(`UPDATE refresh_tokens SET replaced_at = replaced_at - interval '2 minutes' WHERE token_hash = ?`,
		tokenstore.Hash(f.refresh)).Error)
	err := w.during(down, func() error {
		_, err := w.s.Refresh(w.ctx, f.refresh)
		return err
	})
	require.True(w.t, isCode(err, "unauthenticated"), "%s: replay: %v", w.where(), err)
	if f.exchanged {
		w.ended[f.id] = true
	}
	if down {
		w.endSessions("")
	}
}

// revokeOne revokes a session or a family by id (DELETE /me/sessions/:id):
// there while its family has a live refresh token, or its opaque session
// no change ended. With Valkey down only a family can be found, and a bump
// stands in for Valkey.
func (w *sessionWalk) revokeOne(down bool) {
	id := w.pickID()
	if id == "" {
		return
	}
	f := w.family(id)
	_, opaque := w.current[id]
	familyLive, opaqueLive := f != nil && !f.revoked, opaque && !w.ended[id]
	err := w.during(down, func() error { return w.s.RevokeSession(w.ctx, w.uid, id) })
	switch {
	case familyLive || (!down && opaqueLive):
		w.ok(err)
	case down:
		require.True(w.t, isCode(err, "unavailable"), "%s: revoking %s: %v", w.where(), id, err)
		return
	default:
		require.True(w.t, isCode(err, "not_found"), "%s: revoking %s: %v", w.where(), id, err)
		return
	}
	w.ended1(id)
	if down {
		w.endSessions("")
	}
}

// opaqueLogout signs out with an opaque session: it ends alone, every one
// with Valkey down.
func (w *sessionWalk) opaqueLogout(down bool) {
	id := w.pickSession()
	if id == "" {
		return
	}
	w.ok(w.during(down, func() error { return w.s.Logout(core.WithOpaqueSession(w.ctx, id), w.uid, "") }))
	w.ended1(id)
	if down {
		w.endSessions("")
	}
}

// jwtLogout signs a JWT session out with its refresh token on the JWT
// node: no opaque session ends, unless Valkey is down (a bump stands in).
func (w *sessionWalk) jwtLogout(down bool) {
	f := w.pickFamily(func(f *walkFamily) bool { return !f.revoked })
	if f == nil {
		w.jwtLogin()
		f = w.families[len(w.families)-1]
	}
	w.ok(w.during(down, func() error { return w.jwtNode.Logout(w.ctx, w.uid, f.refresh) }))
	f.revoked = true
	if down {
		w.endSessions("")
	}
}

// endAll ends every session, as a JWT caller signing out without its
// refresh token does; with Valkey down the cleanup after the commit fails.
func (w *sessionWalk) endAll(down bool) {
	w.ok(w.during(down, func() error { return w.s.Logout(core.WithSession(w.ctx, "a-jwt-family"), w.uid, "") }))
	w.endedEverything()
}

// revokeOthers ends every session but one. With Valkey down the one kept
// ends too (a bump stands in), and the revocation is left for Valkey to
// run late.
func (w *sessionWalk) revokeOthers(down bool) {
	keep := w.pickID()
	var from int
	w.ok(userTokenVersion(w.s.db.Gorm, w.uid).Row().Scan(&from))
	at := time.Now()
	w.ok(w.during(down, func() error { return w.s.RevokeOtherSessions(w.ctx, w.uid, keep) }))
	w.revokeFamilies(keep)
	if down {
		w.endSessions("")
		w.late = &lateRevoke{keep: keep, from: from, at: at}
		return
	}
	w.endSessions(keep)
}

// revokeOthersRolledBack ends the other opaque sessions and restamps the
// one kept before the commit, which then fails: Valkey does not roll back,
// PostgreSQL does (no bump, the families stay).
func (w *sessionWalk) revokeOthersRolledBack() {
	keep := w.pickID()
	restore := failAudit(w.t, w.s, "user.session.revoke_others", true)
	err := w.s.RevokeOtherSessions(w.ctx, w.uid, keep)
	restore()
	require.Error(w.t, err, w.where())
	w.endSessions(keep)
}

// runLate runs the revocation a caller gave up on, as Valkey would run it
// late: the sessions issued since stay.
func (w *sessionWalk) runLate() {
	l := w.late
	if l == nil {
		return
	}
	late := tokenstore.NewValkey(w.rdb, tokenstore.Options{Now: func() time.Time { return l.at }})
	w.ok(late.Revoke(w.ctx, w.uid, tokenstore.Selection{Keep: l.keep}, tokenstore.Bump{From: l.from, To: l.from + 1}))
}

func (w *sessionWalk) adminPasswordReset() {
	password := adminPassword
	_, err := w.s.UpdateUser(w.ctx, 0, w.uid, UpdateUserInput{Password: &password})
	w.ok(err)
	w.endedEverything()
}

// adminDisable disables the user and enables them again.
func (w *sessionWalk) adminDisable() {
	for _, status := range []string{StatusDisabled, StatusActive} {
		_, err := w.s.UpdateUser(w.ctx, 0, w.uid, UpdateUserInput{Status: &status})
		w.ok(err)
	}
	w.endedEverything()
}

func (w *sessionWalk) adminSecurityReset() {
	w.ok(w.s.ResetSecurity(w.ctx, 0, w.uid))
	w.endedEverything()
}

// cliBump ends every session the way `sub2api admin recover-superadmin`
// does to its target: with PostgreSQL alone, no bus. The nodes notice
// within statusCacheTTL, which the walk does not wait out.
func (w *sessionWalk) cliBump() {
	w.ok(w.cli.ResetSecurity(w.ctx, 0, w.uid))
	for _, n := range []*Service{w.s, w.jwtNode, w.observer} {
		n.forgetStatus(w.uid)
	}
	w.endedEverything()
}

// sessionRace is one user's sessions changed by workers at once. Every
// login and change records the ticks it started and returned at.
type sessionRace struct {
	ctx        context.Context
	s, jwtNode *Service
	uid        int64
	email      string
	clock      atomic.Int64

	mu       sync.Mutex
	sessions []*raceSession
	changes  []raceChange
}

// raceSession is an opaque session a worker signed in; its last token is
// the current one.
type raceSession struct {
	sid    string
	tokens []string
	born   [2]int64
}

// raceChange ended the opaque session only, or every one but keep.
type raceChange struct {
	only, keep string
	at         [2]int64
}

func (c raceChange) ends(sid string) bool {
	if c.only != "" {
		return c.only == sid
	}
	return c.keep != sid
}

// change runs fn and records c when it succeeds.
func (r *sessionRace) change(c raceChange, fn func() error) error {
	start := r.clock.Add(1)
	err := fn()
	c.at = [2]int64{start, r.clock.Add(1)}
	if err == nil {
		r.mu.Lock()
		r.changes = append(r.changes, c)
		r.mu.Unlock()
	}
	return err
}

// work runs steps changes picked by seed, on sessions of its own, and
// returns their names.
func (r *sessionRace) work(t *testing.T, seed uint64, steps int) []string {
	rng := rand.New(rand.NewPCG(seed, 7))
	var own []*raceSession
	var trail []string
	for range steps {
		some := "" // one of its sessions, if any
		if len(own) > 0 {
			some = own[rng.IntN(len(own))].sid
		}
		var err error
		switch op := rng.IntN(6); op {
		case 0:
			trail = append(trail, "login")
			x := &raceSession{born: [2]int64{r.clock.Add(1), 0}}
			var p *TokenPair
			if p, err = r.s.Login(r.ctx, r.email, adminPassword, testIP); err != nil {
				break
			}
			x.born[1] = r.clock.Add(1)
			// Read as stored: a change running alongside may have ended
			// it already, and one that removed it leaves nothing to check.
			var stored tokenstore.Session
			if stored, err = r.s.sessions.Lookup(r.ctx, p.AccessToken); err != nil {
				if errors.Is(err, tokenstore.ErrNotFound) {
					err = nil
				}
				break
			}
			x.sid, x.tokens = stored.ID, []string{p.AccessToken}
			own = append(own, x)
			r.mu.Lock()
			r.sessions = append(r.sessions, x)
			r.mu.Unlock()
		case 1:
			trail = append(trail, "rotate")
			if len(own) == 0 {
				continue
			}
			x := own[rng.IntN(len(own))]
			var token string
			if token, err = r.s.sessions.Rotate(r.ctx, r.uid, x.sid); err == nil {
				x.tokens = append(x.tokens, token)
			} else if errors.Is(err, tokenstore.ErrNotFound) {
				err = nil
			}
		case 2:
			trail = append(trail, "revoke others")
			err = r.change(raceChange{keep: some}, func() error { return r.s.RevokeOtherSessions(r.ctx, r.uid, some) })
		case 3:
			trail = append(trail, "JWT logout")
			var p *TokenPair
			if p, err = r.jwtNode.Login(r.ctx, r.email, adminPassword, testIP); err == nil {
				err = r.jwtNode.Logout(r.ctx, r.uid, p.RefreshToken)
			}
		case 4:
			trail = append(trail, "end all")
			err = r.change(raceChange{}, func() error { return r.s.Logout(core.WithSession(r.ctx, "a-jwt-family"), r.uid, "") })
		case 5:
			trail = append(trail, "opaque logout")
			if some == "" {
				continue
			}
			err = r.change(raceChange{only: some}, func() error {
				return r.s.Logout(core.WithOpaqueSession(r.ctx, some), r.uid, "")
			})
		}
		assert.NoError(t, err, "%q", trail)
	}
	return trail
}

// The same at once: whatever runs alongside (logins, rotations, bumps that
// keep sessions or end them all), a change that started after a login
// returned ends that session, every token of it, on both nodes; one that
// returned before a login started leaves it signed in.
func TestEndedSessionsStayEndedConcurrently(t *testing.T) {
	t.Setenv(IssueEnv, IssueOpaque)
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	t.Setenv(IssueEnv, IssueJWT)
	jwtNode := New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: e.authz, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	for round := range 3 {
		email := fmt.Sprintf("race-%d@example.com", round)
		u, err := s.CreateUser(ctx, root, CreateUserInput{Email: email, Password: adminPassword})
		require.NoError(t, err)
		r := &sessionRace{ctx: ctx, s: s, jwtNode: jwtNode, uid: u.ID, email: email}
		trails := make([][]string, 6)
		var wg sync.WaitGroup
		for worker := range trails {
			wg.Go(func() { trails[worker] = r.work(t, uint64(round*len(trails)+worker), 8) })
		}
		wg.Wait()
		for _, x := range r.sessions {
			mustEnd, mayEnd := false, false
			for _, c := range r.changes {
				if c.ends(x.sid) {
					mustEnd = mustEnd || c.at[0] > x.born[1]
					mayEnd = mayEnd || c.at[1] > x.born[0]
				}
			}
			for _, n := range []*Service{jwtNode, s} {
				switch {
				case mustEnd:
					for _, tok := range x.tokens {
						_, err := n.VerifyAccessToken(ctx, tok)
						require.True(t, isCode(err, "unauthenticated"), "round %d %+v %+v %q: an ended session works: %v", round, x, r.changes, trails, err)
					}
				case !mayEnd:
					_, err := n.VerifyAccessToken(ctx, x.tokens[len(x.tokens)-1])
					require.NoError(t, err, "round %d %+v %+v %q: a session no change ended is rejected", round, x, r.changes, trails)
				}
			}
		}
	}
}

package iam

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/authztest"
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
	begin, err := s.BeginPasskeyLogin(ctx, r, testIP)
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
	require.NoError(t, s.sessions.Revoke(ctx, uid, tokenstore.Selection{Only: idA}, 0))
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
	require.NoError(t, s.sessions.Revoke(ctx, uid, tokenstore.Selection{Only: "none"}, tv+1))
	live(t, s, a.AccessToken)

	cli := New(Deps{DB: s.db, Config: e.cfg, Authz: e.authz, BcryptCost: bcrypt.MinCost})
	require.NoError(t, cli.ChangePassword(ctx, uid, "", "another-password-1"))
	s.forgetStatus(uid)
	ended(t, s, a.AccessToken)
}

// failSuperuserLoginAudit makes PostgreSQL fail the statements or the commit of a
// superuser login (its audit row): what fails a login after the checks.
func failSuperuserLoginAudit(t *testing.T, s *Service, atCommit bool) func() {
	t.Helper()
	trigger := `CREATE TRIGGER fail_login BEFORE INSERT ON audit_logs FOR EACH ROW
		WHEN (NEW.action = 'auth.superadmin_login') EXECUTE FUNCTION fail_login()`
	if atCommit {
		trigger = `CREATE CONSTRAINT TRIGGER fail_login AFTER INSERT ON audit_logs DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (NEW.action = 'auth.superadmin_login') EXECUTE FUNCTION fail_login()`
	}
	require.NoError(t, s.db.Gorm.Exec(`CREATE OR REPLACE FUNCTION fail_login() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'login fails here'; END $$`).Error)
	require.NoError(t, s.db.Gorm.Exec(trigger).Error)
	return func() { require.NoError(t, s.db.Gorm.Exec(`DROP TRIGGER fail_login ON audit_logs`).Error) }
}

// A login whose transaction fails leaves no opaque session behind, and a
// second step that fails before its end leaves the flow to try again.
func TestFailedLoginLeavesNothingBehind(t *testing.T) {
	t.Setenv(IssueEnv, "")
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	issue(t, s, IssueOpaque)

	restore := failSuperuserLoginAudit(t, s, true)
	_, err := s.Login(ctx, adminEmail, adminPassword, testIP)
	require.Error(t, err)
	restore()
	list, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, list)

	secret, _ := enableTwoFactor(t, s, id)
	step := mustLogin(t, s, adminEmail, adminPassword)
	require.True(t, step.TwoFactorRequired)
	restore = failSuperuserLoginAudit(t, s, false)
	clearLastStep(t, s, id)
	_, err = s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.ErrorContains(t, err, "login fails here")
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

	// The registry unreadable: no switch to s2s, the way back still works.
	s.nodes = nodesDown{}
	_, err = s.SetIssue(ctx, root, IssueJWT)
	require.NoError(t, err)
	_, err = s.SetIssue(ctx, root, IssueOpaque)
	assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status)
	st, err = s.IssueStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, IssueJWT, st.Effective)
	assert.Nil(t, st.Behind)
}

// nodesDown is a registry Valkey does not answer for.
type nodesDown struct{}

func (nodesDown) LiveNodes(context.Context) ([]core.NodeStatus, error) {
	return nil, tokenstore.ErrUnavailable
}

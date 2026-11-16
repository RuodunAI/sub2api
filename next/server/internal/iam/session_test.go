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

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Sessions in Valkey (CONTRACTS §100): revocation at once, step-up
// rotation, the flows, the outage, and the token_version stamps that end
// sessions without Valkey.

// rootID is the bootstrap super admin.
func rootID(t *testing.T, s *Service) int64 {
	t.Helper()
	var id int64
	if err := userByEmail(s.db.Gorm, adminEmail, "id").Row().Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// sid is the session of a live token.
func sid(t *testing.T, s *Service, token string) string {
	t.Helper()
	_, id, err := s.VerifySession(context.Background(), token)
	if err != nil || id == "" {
		t.Fatalf("session of %q: %q %v", token, id, err)
	}
	return id
}

// asSession is ctx of a request made with the session of token.
func asSession(t *testing.T, s *Service, ctx context.Context, token string) context.Context {
	t.Helper()
	return core.WithSession(ctx, sid(t, s, token))
}

func live(t *testing.T, s *Service, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if _, _, err := s.VerifySession(context.Background(), tok); err != nil {
			t.Fatalf("%.12s…: %v", tok, err)
		}
	}
}

func ended(t *testing.T, s *Service, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if _, _, err := s.VerifySession(context.Background(), tok); !isCode(err, "unauthenticated") {
			t.Fatalf("%.12s… still works: %v", tok, err)
		}
	}
}

// sessKey is the Valkey key of a token.
func sessKey(token string) string { return "s2a:sess:" + tokenstore.Hash(token) }

// A token that is not a session token is no session, without a lookup.
func TestOnlySessionTokensSignIn(t *testing.T) {
	t.Parallel()
	s := setupAdmin(t).svc
	ended(t, s, "", "eyJhbGciOiJIUzI1NiJ9.e30.x", tokenstore.SessionPrefix+"unknown")
}

// Revocation ends a session at once and keeps the others. A user still at
// token_version 0 keeps the session revoke-others keeps.
func TestRevocation(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	u, err := s.CreateUser(ctx, rootID(t, s), CreateUserInput{Email: "bob@example.com", Password: adminPassword})
	require.NoError(t, err)
	a := mustLogin(t, s, u.Email, adminPassword)
	b := mustLogin(t, s, u.Email, adminPassword)
	c := mustLogin(t, s, u.Email, adminPassword)
	id := u.ID
	var tv int
	require.NoError(t, userTokenVersion(s.db.Gorm, id).Row().Scan(&tv))
	require.Zero(t, tv)

	// One session ends alone, with no bump.
	require.NoError(t, s.RevokeSession(ctx, id, sid(t, s, a.AccessToken)))
	ended(t, s, a.AccessToken)
	live(t, s, b.AccessToken, c.AccessToken)
	require.NoError(t, userTokenVersion(s.db.Gorm, id).Row().Scan(&tv))
	assert.Zero(t, tv)
	err = s.RevokeSession(ctx, rootID(t, s), sid(t, s, b.AccessToken))
	assert.True(t, isCode(err, "not_found"), "revoked another user's session: %v", err)
	live(t, s, b.AccessToken)

	renewed, err := s.RevokeOtherSessions(ctx, id, sid(t, s, b.AccessToken), b.AccessToken)
	require.NoError(t, err)
	ended(t, s, c.AccessToken)
	live(t, s, b.AccessToken, renewed) // b for the grace of the rotation
	list, err := s.Sessions(ctx, id, sid(t, s, b.AccessToken))
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Current)

	// Changing the password keeps the caller's session only.
	d := mustLogin(t, s, u.Email, adminPassword)
	require.NoError(t, s.ChangePassword(ctx, id, sid(t, s, b.AccessToken), "another-password-1"))
	ended(t, s, d.AccessToken)
	live(t, s, b.AccessToken, renewed)

	// A token_version bump that did not go through Valkey (the server CLI)
	// still ends them, and they leave the list.
	require.NoError(t, s.db.Gorm.Exec(`UPDATE users SET token_version = token_version + 1 WHERE id = ?`, id).Error)
	s.forgetStatus(id)
	ended(t, s, b.AccessToken, renewed)
	stored, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

// Logging out ends the caller's session only, even when it is gone
// already.
func TestLogoutEndsOnlyItself(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	a := mustLogin(t, s, adminEmail, adminPassword)
	b := mustLogin(t, s, adminEmail, adminPassword)
	idA, uid := sid(t, s, a.AccessToken), a.User.ID
	require.NoError(t, s.Logout(core.WithSession(ctx, idA), uid))
	ended(t, s, a.AccessToken)
	live(t, s, b.AccessToken)
	require.NoError(t, s.Logout(core.WithSession(ctx, idA), uid))
	live(t, s, b.AccessToken)
}

// Every way an operator ends a user's sessions ends them.
func TestSessionsEndWithTheUser(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	u, err := s.CreateUser(ctx, root, CreateUserInput{Email: "bob@example.com", Password: "password-1"})
	require.NoError(t, err)
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
		p := mustLogin(t, s, u.Email, password)
		require.NoError(t, step.run(), step.name)
		ended(t, s, p.AccessToken)
		password = pw
	}
	active := StatusActive
	_, err = s.UpdateUser(ctx, root, u.ID, UpdateUserInput{Status: &active})
	require.NoError(t, err)
	p := mustLogin(t, s, u.Email, pw)
	require.NoError(t, s.DeleteUser(ctx, root, u.ID))
	ended(t, s, p.AccessToken)

	// Resetting one's own factors keeps the session doing it.
	mine := mustLogin(t, s, adminEmail, adminPassword)
	other := mustLogin(t, s, adminEmail, adminPassword)
	require.NoError(t, s.ResetSecurity(asSession(t, s, ctx, mine.AccessToken), root, root))
	live(t, s, mine.AccessToken)
	ended(t, s, other.AccessToken)
}

// A second node keeps its own status cache: an admin's password reset on
// node 1 must reach it at once, not after statusCacheTTL.
func TestAdminPasswordResetBroadcasts(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := mustLogin(t, s, adminEmail, adminPassword).User.ID
	pw, reset := "password-1", "password-2"
	u, err := s.CreateUser(ctx, root, CreateUserInput{Email: "bob@example.com", Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	az2 := authz.New(authz.Deps{DB: s.db, Bus: e.bus})
	if err := az2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	node2 := New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: az2, Bus: e.bus, BcryptCost: bcrypt.MinCost})

	p := mustLogin(t, s, u.Email, pw)
	if _, _, err := node2.VerifySession(ctx, p.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUser(withProof(t, s, ctx, root, core.StepUpUserPasswordReset), root, u.ID, UpdateUserInput{Password: &reset}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := node2.VerifySession(ctx, p.AccessToken); !isCode(err, "unauthenticated") {
		t.Fatalf("node 2 still accepts the old token: %v", err)
	}
}

// ChangePassword takes only the user lock and UpdateUser{Status} takes the
// authz lock first, then the user lock: run together they must not deadlock.
func TestChangePasswordAndStatusUpdateDoNotDeadlock(t *testing.T) {
	e := setupAdmin(t)
	s := e.svc
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := mustLogin(t, s, adminEmail, adminPassword).User.ID
	pws := [2]string{"password-1", "password-2"}
	u, err := s.CreateUser(ctx, root, CreateUserInput{Email: "carol@example.com", Password: pws[0]})
	if err != nil {
		t.Fatal(err)
	}
	const rounds = 100
	errs := make(chan error, 2*rounds)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range rounds {
			errs <- s.ChangePassword(ctx, u.ID, "", pws[(i+1)%2])
		}
	}()
	go func() {
		defer wg.Done()
		statuses := [2]string{StatusDisabled, StatusActive}
		for i := range rounds {
			_, err := s.UpdateUser(ctx, root, u.ID, UpdateUserInput{Status: &statuses[i%2]})
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// superadminLogins reads the auth.superadmin_login audit rows as user, method
// and ip.
func superadminLogins(t *testing.T, s *Service) []string {
	t.Helper()
	rows, err := s.db.SQL.QueryContext(context.Background(), `SELECT user_id || ' ' || (detail->>'method') || ' ' || (detail->>'ip') || ' ' || ip
		FROM audit_logs WHERE action = 'auth.superadmin_login' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := testutil.Column[string](rows)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSuperadminLoginIsAudited(t *testing.T) {
	t.Parallel()
	s, ctx := setupAdmin(t).svc, context.Background()
	login := context.WithValue(ctx, sessionMetaKey{}, sessionMeta{method: loginMethodPassword, ip: testIP})
	root, err := s.Login(login, adminEmail, adminPassword, testIP)
	if err != nil {
		t.Fatal(err)
	}
	want := strconv.FormatInt(root.User.ID, 10) + " password " + testIP + " " + testIP
	if got := superadminLogins(t, s); len(got) != 1 || got[0] != want {
		t.Fatalf("after super admin login: %q, want [%q]", got, want)
	}

	// An ordinary user's login is not audited.
	if _, err := s.CreateUser(ctx, root.User.ID, CreateUserInput{Email: "plain@example.com", Password: "password-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(login, "plain@example.com", "password-1", testIP); err != nil {
		t.Fatal(err)
	}
	if got := superadminLogins(t, s); len(got) != 1 {
		t.Fatalf("after a user login: %q", got)
	}
}

// A session that verifies gets a new token of the same session: the proof,
// bound to the session, is spent with the new token; the old one works for
// the requests already sent with it.
func TestStepUpRotatesTheSession(t *testing.T) {
	k := newKeyEnv(t)
	old := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	path := "/users/" + strconv.FormatInt(k.userID, 10)
	code, out := k.call(old, "POST", "/me/verify", map[string]any{"scope": core.StepUpUserDelete, "context": "DELETE /api/v1" + path,
		"method": "password", "password": adminPassword})
	require.Equal(t, 200, code, out)
	data := out["data"].(map[string]any)
	fresh, _ := data["access_token"].(string)
	require.True(t, strings.HasPrefix(fresh, tokenstore.SessionPrefix) && fresh != old, "verify gave access token %q", fresh)
	require.Equal(t, sid(t, k.s, old), sid(t, k.s, fresh), "the rotation changed the session id")
	code, out = k.callProof(fresh, data["proof"].(string), "DELETE", path, nil)
	require.Equal(t, 204, code, out)
	k.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	ended(t, k.s, old)
	live(t, k.s, fresh)
}

// Flows are taken once, by their owner only.
func TestFlowsTakenOnce(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	secret, _ := enableTwoFactor(t, s, id)
	step := mustLogin(t, s, adminEmail, adminPassword)
	clearLastStep(t, s, id)
	result, err := s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.AccessToken, tokenstore.SessionPrefix))
	clearLastStep(t, s, id)
	_, err = s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	assert.True(t, isCode(err, "invalid_argument") && !isBadSecondFactor(err), "flow reused: %v", err)

	token, err := s.createFlow(ctx, flowTOTPSetup, id, totpSetupPayload{Sealed: "x"})
	require.NoError(t, err)
	var p totpSetupPayload
	_, err = s.peekFlow(ctx, token, flowTOTPSetup, id+1, &p)
	assert.Equal(t, errFlowExpired, err, "another user's flow")
	owner, err := s.takeFlow(ctx, token, flowTOTPSetup, id, &p)
	require.NoError(t, err)
	assert.Equal(t, id, owner)
	assert.Equal(t, "x", p.Sealed)
}

// Valkey down: sessions and logins fail with 503 (never 401, which signs
// the console out). An operator still disables a user: the bump ends the
// user's sessions without Valkey.
func TestValkeyDownIsAnOutage(t *testing.T) {
	k := newKeyEnv(t)
	ctx := context.Background()
	token := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	victim := mustLogin(t, k.s, "key-user@example.com", adminPassword).AccessToken
	var valkey outage
	valkey.on(k.env, k.s)
	require.NoError(t, valkey.during(func() error {
		_, _, err := k.s.VerifySession(ctx, token)
		assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status, "verify: %v", err)
		code, out := k.call(token, "GET", "/me", nil)
		assert.Equal(t, http.StatusServiceUnavailable, code, out)
		assert.Equal(t, "unavailable", errCode(out))
		_, err = k.s.Login(ctx, adminEmail, adminPassword, testIP)
		assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status, "login: %v", err)
		disabled := StatusDisabled
		_, err = k.s.UpdateUser(ctx, rootID(t, k.s), k.userID, UpdateUserInput{Status: &disabled})
		return err
	}))
	ended(t, k.s, victim)
	live(t, k.s, token)
}

// PostgreSQL failing while a session is checked (a status cache miss) is an
// outage too: the server error as it is, never a 401 that ends the console
// session, which is still good once the database is back.
func TestDatabaseDownIsAnOutage(t *testing.T) {
	k := newKeyEnv(t)
	token := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	k.s.clearStatusCache()
	require.NoError(t, k.s.db.Gorm.Exec(`ALTER TABLE users RENAME TO users_away`).Error)
	code, out := k.call(token, "GET", "/me", nil)
	require.NoError(t, k.s.db.Gorm.Exec(`ALTER TABLE users_away RENAME TO users`).Error)
	assert.Equal(t, http.StatusInternalServerError, code, out)
	live(t, k.s, token)
}

// A passkey login issues a session too, and a login whose transaction
// fails leaves none behind.
func TestPasskeyLoginIssuesASession(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)
	r := consoleRequest(consoleOrigin, consoleHost)
	a := newAuthenticator(t, consoleOrigin, consoleHost)
	addPasskey(t, s, a, id)
	begin, err := s.BeginPasskeyLogin(ctx, r, testIP)
	require.NoError(t, err)
	restore := failAudit(t, s, "auth.superadmin_login", true)
	_, err = s.FinishPasskeyLogin(withLoginMethod(ctx, loginMethodPasskey), r, begin.FlowToken, a.get(t, begin), testIP)
	restore()
	require.Error(t, err)
	list, err := s.sessions.List(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, list)

	begin, err = s.BeginPasskeyLogin(ctx, r, testIP)
	require.NoError(t, err)
	credential := a.get(t, begin)
	result, err := s.FinishPasskeyLogin(withLoginMethod(ctx, loginMethodPasskey), r, begin.FlowToken, credential, testIP)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.AccessToken, tokenstore.SessionPrefix))
	sessions, err := s.Sessions(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, loginMethodPasskey, sessions[0].Method)
	_, err = s.FinishPasskeyLogin(ctx, r, begin.FlowToken, credential, testIP)
	assert.True(t, isCode(err, "invalid_argument"), "replay: %v", err)
}

// A bump that keeps the caller's session keeps the token a step-up rotation
// left in its grace too: a request still carrying it does not end the
// session. A token left behind that is stale ends alone; a second rotation
// ends the first token at once.
func TestKeptSessionKeepsItsRotatedToken(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	t0 := mustLogin(t, s, adminEmail, adminPassword)
	id, uid := sid(t, s, t0.AccessToken), t0.User.ID
	t1, err := s.sessions.Rotate(ctx, uid, id, t0.AccessToken)
	require.NoError(t, err)
	require.NoError(t, s.ChangePassword(ctx, uid, id, "another-password-1"))
	live(t, s, t0.AccessToken, t1)

	t2, err := s.sessions.Rotate(ctx, uid, id, t1)
	require.NoError(t, err)
	ended(t, s, t0.AccessToken)
	e.redis.HSet(sessKey(t1), "tv", "0")
	ended(t, s, t1)
	live(t, s, t2)
}

// A stamp a rolled-back bump left on a kept session (one ahead of the row)
// does not survive a later bump that ends every session without Valkey
// (the server CLI).
func TestRolledBackStampDoesNotSurviveABump(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	a := mustLogin(t, s, adminEmail, adminPassword)
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

// A login whose transaction fails leaves no session behind, and a second
// step that fails before its end leaves the flow to try again.
func TestFailedLoginLeavesNothingBehind(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	id := rootID(t, s)

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
	result, err := s.LoginSecondFactor(ctx, step.FlowToken, codeAt(t, secret, time.Now()), testIP)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.AccessToken, tokenstore.SessionPrefix))

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

func (rotateFails) Rotate(context.Context, int64, string, string) (string, error) {
	return "", tokenstore.ErrUnavailable
}

// A verification whose rotation fails is an outage that leaves no audit
// and no proof behind.
func TestFailedRotationLeavesNothingBehind(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	s.sessions = rotateFails{s.sessions}
	p := mustLogin(t, s, adminEmail, adminPassword)
	audits := func() (n int) {
		require.NoError(t, s.db.Gorm.Raw(`SELECT count(*) FROM audit_logs WHERE action = 'user.security_verify'`).Row().Scan(&n))
		return n
	}
	before := audits()
	_, err := s.VerifyStepUp(asSession(t, s, ctx, p.AccessToken), consoleRequest(consoleOrigin, consoleHost), p.User.ID,
		VerifyInput{Scope: core.StepUpUserDelete, Context: "DELETE /api/v1/users/1", Method: methodPassword, Password: adminPassword}, testIP)
	assert.Equal(t, http.StatusServiceUnavailable, core.AsError(err).Status)
	assert.Equal(t, before, audits())
	for _, key := range e.redis.Keys() {
		assert.False(t, strings.HasPrefix(key, "s2a:flow:"+flowStepUpProof), key)
	}
}

// endWithoutValkey ends every session of uid the way the server CLI does: a
// token_version bump that never reaches Valkey, which keeps the sessions
// there (as when the cleanup after the commit fails).
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

// on gives s session and flow stores on e's Valkey that fails while o is down. A
// token a rotation leaves behind lasts the test.
func (o *outage) on(e *env, s *Service) {
	rdb := redis.NewClient(&redis.Options{Addr: e.redis.Addr()})
	rdb.AddHook(o)
	v := tokenstore.NewValkey(rdb, tokenstore.Options{RotateGrace: time.Hour})
	s.sessions, s.flows = v, v
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
// bump that keeps another session: the restamp touches the kept one only.
func TestEndedSessionStaysEnded(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	keeps := []struct {
		name string
		run  func(keep, password string) error
	}{
		{"revoke others", func(keep, _ string) error { _, err := s.RevokeOtherSessions(ctx, uid, keep, ""); return err }},
		{"password change", func(keep, password string) error { return s.ChangePassword(ctx, uid, keep, password) }},
	}
	password := adminPassword
	for i, keep := range keeps {
		z := mustLogin(t, s, adminEmail, password)
		password = "another-password-" + strconv.Itoa(i)
		endWithoutValkey(t, e, uid, password)
		k := mustLogin(t, s, adminEmail, password)
		require.NoError(t, keep.run(sid(t, s, k.AccessToken), password), keep.name)
		s.forgetStatus(uid)
		ended(t, s, z.AccessToken)
		live(t, s, k.AccessToken)
	}
}

// Cleaning up after ending every session leaves the sessions that signed
// in since the commit.
func TestCleanupKeepsLaterLogins(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	old := mustLogin(t, s, adminEmail, adminPassword)
	endWithoutValkey(t, e, uid, "another-password-1")
	fresh := mustLogin(t, s, adminEmail, "another-password-1")
	s.sessionsEnded(ctx, uid)
	live(t, s, fresh.AccessToken)
	assert.False(t, e.redis.Exists(sessKey(old.AccessToken)))
}

// A bump that keeps a session but cannot restamp (Valkey down) also ends a
// session that a rolled-back bump stamped one ahead.
func TestKeepingBumpWithValkeyDownEndsAheadStamps(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	a := mustLogin(t, s, adminEmail, adminPassword)
	b := mustLogin(t, s, adminEmail, adminPassword)
	keep := sid(t, s, b.AccessToken)
	var tv int
	require.NoError(t, userTokenVersion(s.db.Gorm, uid).Row().Scan(&tv))
	e.redis.HSet(sessKey(a.AccessToken), "tv", strconv.Itoa(tv+1))
	var valkey outage
	valkey.on(e, s)
	require.NoError(t, valkey.during(func() error { _, err := s.RevokeOtherSessions(ctx, uid, keep, ""); return err }))
	s.forgetStatus(uid)
	ended(t, s, a.AccessToken)
}

// The sessions list leaves out the sessions a bump ended that are still in
// Valkey, and revoking one finds no such session.
func TestSessionsLeaveOutEndedOnes(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	uid := rootID(t, s)
	old := sid(t, s, mustLogin(t, s, adminEmail, adminPassword).AccessToken)
	endWithoutValkey(t, e, uid, "another-password-1")
	fresh := mustLogin(t, s, adminEmail, "another-password-1")
	list, err := s.Sessions(ctx, uid, "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, sid(t, s, fresh.AccessToken), list[0].ID)
	err = s.RevokeSession(ctx, uid, old)
	assert.True(t, isCode(err, "not_found"), "revoking an ended session: %v", err)
}

// keepEnded is a session store whose Drop does nothing: a node that finds
// a token ended without removing it, so a later change still has the
// chance to revive it, which it must not.
type keepEnded struct{ tokenstore.SessionStore }

func (keepEnded) Drop(context.Context, string, tokenstore.Session, int) (bool, error) {
	return true, nil
}

// walkNodes are the nodes the walks run on: s and other serve the console
// and share a bus; observer checks ended tokens without removing them; cli
// is the server CLI (no Valkey, no bus). valkey fails the session stores of
// s and other on demand.
type walkNodes struct {
	t                       *testing.T
	ctx                     context.Context
	s, other, observer, cli *Service
	valkey                  *outage
	rdb                     redis.Cmdable
}

// sessionWalk is one user's sessions through a walk of changes, and what
// the walk expects of every token issued: one is live unless a committed
// change ended its session, or a second rotation ended it.
type sessionWalk struct {
	*walkNodes
	rng   *rand.Rand
	name  string
	uid   int64
	email string
	trail []string

	tokens  []walkToken
	sids    []string          // the sessions, in the order issued
	current map[string]string // session -> its token
	prev    map[string]string // session -> the token its last rotation left
	ended   map[string]bool   // sessions a committed change ended
	rotated map[string]bool   // tokens a second rotation ended
	late    *lateRevoke
}

type walkToken struct{ value, sid string }

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
	{"revoke one", 1, func(w *sessionWalk) { w.revokeOne(false) }},
	{"revoke one, Valkey down", 1, func(w *sessionWalk) { w.revokeOne(true) }},
	{"logout", 1, func(w *sessionWalk) { w.logout(false) }},
	{"logout, Valkey down", 1, func(w *sessionWalk) { w.logout(true) }},
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
// rotations, single and bulk revocations, logouts, operators' changes, the
// server CLI, rolled-back restamps), with Valkey failing meanwhile and a
// revocation Valkey runs late: a session stays ended once a committed
// change ended it, every token of it, and a session no change ended stays
// signed in, on both nodes. Each step is checked at once, an ended token
// without removing it (a later change must not revive it), and every
// token again at the end of the walk on both nodes. The walks are fixed
// pseudo-random sequences, from a user at token_version 0 and from one
// bumped; a failure names the one that broke it.
func TestEndedSessionsStayEndedInAnyOrder(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	node := func() *Service {
		return New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: e.authz, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	}
	nodes := &walkNodes{t: t, ctx: ctx, s: s, other: node(), observer: node(), valkey: &outage{},
		cli: New(Deps{DB: s.db, Config: e.cfg, Authz: e.authz, BcryptCost: bcrypt.MinCost}),
		rdb: redis.NewClient(&redis.Options{Addr: e.redis.Addr()})}
	nodes.valkey.on(e, s)
	nodes.valkey.on(e, nodes.other)
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
	w.check(w.other)
	w.check(w.s)
}

// check verifies every token as the walk expects it: a live one on both
// nodes, an ended one on ended (it removes the ended tokens unless it is
// the observer).
func (w *sessionWalk) check(ended *Service) {
	w.t.Helper()
	for _, tk := range w.tokens {
		if !w.ended[tk.sid] && !w.rotated[tk.value] {
			for _, n := range []*Service{w.s, w.other} {
				_, _, err := n.VerifySession(w.ctx, tk.value)
				require.NoError(w.t, err, "%s: a token of a session no change ended is rejected", w.where())
			}
			continue
		}
		_, _, err := ended.VerifySession(w.ctx, tk.value)
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

// issued records a token of the session id: a rotation when the session
// has one already, which ends the token the last one left.
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

// endSessions records that every session but keep ended.
func (w *sessionWalk) endSessions(keep string) {
	for _, id := range w.sids {
		if id != keep {
			w.ended[id] = true
		}
	}
}

// pickSession is a session issued so far, "" before any.
func (w *sessionWalk) pickSession() string {
	if len(w.sids) == 0 {
		return ""
	}
	return w.sids[w.rng.IntN(len(w.sids))]
}

func (w *sessionWalk) login() {
	p, err := w.s.Login(w.ctx, w.email, adminPassword, testIP)
	w.ok(err)
	_, id, err := w.s.VerifySession(w.ctx, p.AccessToken)
	w.ok(err)
	w.issued(id, p.AccessToken)
}

// rotate gives a session a new token, as a step-up does; one Valkey no
// longer has is not found.
func (w *sessionWalk) rotate() {
	id := w.pickSession()
	if id == "" {
		return
	}
	token, err := w.s.sessions.Rotate(w.ctx, w.uid, id, w.current[id])
	if w.ended[id] && errors.Is(err, tokenstore.ErrNotFound) {
		return
	}
	w.ok(err)
	w.issued(id, token)
}

// revokeOne revokes a session by id (DELETE /me/sessions/:id): there while
// no change ended it. With Valkey down nothing is found and nothing ends.
func (w *sessionWalk) revokeOne(down bool) {
	id := w.pickSession()
	if id == "" {
		return
	}
	err := w.during(down, func() error { return w.s.RevokeSession(w.ctx, w.uid, id) })
	switch {
	case down:
		require.True(w.t, isCode(err, "unavailable"), "%s: revoking %s: %v", w.where(), id, err)
	case w.ended[id]:
		require.True(w.t, isCode(err, "not_found"), "%s: revoking %s: %v", w.where(), id, err)
	default:
		w.ok(err)
		w.ended[id] = true
	}
}

// logout signs a session out: it ends alone, every one with Valkey down (a
// bump stands in).
func (w *sessionWalk) logout(down bool) {
	id := w.pickSession()
	if id == "" {
		return
	}
	w.ok(w.during(down, func() error { return w.s.Logout(core.WithSession(w.ctx, id), w.uid) }))
	w.ended[id] = true
	if down {
		w.endSessions("")
	}
}

// endAll ends every session, as a password change keeping none does; with
// Valkey down the cleanup after the commit fails.
func (w *sessionWalk) endAll(down bool) {
	w.ok(w.during(down, func() error { return w.s.ChangePassword(w.ctx, w.uid, "", adminPassword) }))
	w.endSessions("")
}

// revokeOthers ends every session but one. With Valkey down the one kept
// ends too (a bump stands in), and the revocation is left for Valkey to
// run late.
func (w *sessionWalk) revokeOthers(down bool) {
	keep := w.pickSession()
	var from int
	w.ok(userTokenVersion(w.s.db.Gorm, w.uid).Row().Scan(&from))
	at := time.Now()
	var renewed string
	w.ok(w.during(down, func() (err error) {
		renewed, err = w.s.RevokeOtherSessions(w.ctx, w.uid, keep, w.current[keep])
		return err
	}))
	if down {
		w.endSessions("")
		w.late = &lateRevoke{keep: keep, from: from, at: at}
		return
	}
	w.endSessions(keep)
	if renewed != "" {
		w.issued(keep, renewed)
	}
}

// revokeOthersRolledBack ends the other sessions and restamps the one kept
// before the commit, which then fails: Valkey does not roll back,
// PostgreSQL does (no bump).
func (w *sessionWalk) revokeOthersRolledBack() {
	keep := w.pickSession()
	restore := failAudit(w.t, w.s, "user.session.revoke_others", true)
	_, err := w.s.RevokeOtherSessions(w.ctx, w.uid, keep, w.current[keep])
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
	w.endSessions("")
}

// adminDisable disables the user and enables them again.
func (w *sessionWalk) adminDisable() {
	for _, status := range []string{StatusDisabled, StatusActive} {
		_, err := w.s.UpdateUser(w.ctx, 0, w.uid, UpdateUserInput{Status: &status})
		w.ok(err)
	}
	w.endSessions("")
}

func (w *sessionWalk) adminSecurityReset() {
	w.ok(w.s.ResetSecurity(w.ctx, 0, w.uid))
	w.endSessions("")
}

// cliBump ends every session the way `sub2api admin recover-superadmin`
// does to its target: with PostgreSQL alone, no bus. The nodes notice
// within statusCacheTTL, which the walk does not wait out.
func (w *sessionWalk) cliBump() {
	w.ok(w.cli.ResetSecurity(w.ctx, 0, w.uid))
	for _, n := range []*Service{w.s, w.other, w.observer} {
		n.forgetStatus(w.uid)
	}
	w.endSessions("")
}

// sessionRace is one user's sessions changed by workers at once. Every
// login and change records the ticks it started and returned at.
type sessionRace struct {
	ctx      context.Context
	s, other *Service
	uid      int64
	email    string
	clock    atomic.Int64

	mu       sync.Mutex
	sessions []*raceSession
	changes  []raceChange
}

// raceSession is a session a worker signed in; its last token is the
// current one.
type raceSession struct {
	sid    string
	tokens []string
	born   [2]int64
}

// current is the token x was last given, "" without x.
func current(x *raceSession) string {
	if x == nil {
		return ""
	}
	return x.tokens[len(x.tokens)-1]
}

// raceChange ended the session only, or every one but keep.
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
		some, mine := "", (*raceSession)(nil) // one of its sessions, if any
		if len(own) > 0 {
			mine = own[rng.IntN(len(own))]
			some = mine.sid
		}
		var err error
		switch op := rng.IntN(6); op {
		case 0:
			trail = append(trail, "login")
			x := &raceSession{born: [2]int64{r.clock.Add(1), 0}}
			var p *LoginResult
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
			if token, err = r.s.sessions.Rotate(r.ctx, r.uid, x.sid, x.tokens[len(x.tokens)-1]); err == nil {
				x.tokens = append(x.tokens, token)
			} else if errors.Is(err, tokenstore.ErrNotFound) {
				err = nil
			}
		case 2:
			trail = append(trail, "revoke others")
			err = r.change(raceChange{keep: some}, func() error {
				token, err := r.s.RevokeOtherSessions(r.ctx, r.uid, some, current(mine))
				if token != "" {
					mine.tokens = append(mine.tokens, token)
				}
				return err
			})
		case 3:
			trail = append(trail, "revoke one")
			if some == "" {
				continue
			}
			// Another worker's change may have ended it already.
			if err = r.change(raceChange{only: some}, func() error { return r.s.RevokeSession(r.ctx, r.uid, some) }); isCode(err, "not_found") {
				err = nil
			}
		case 4:
			// An operator's reset: a password change would fail the
			// logins running alongside.
			trail = append(trail, "end all")
			err = r.change(raceChange{}, func() error { return r.s.ResetSecurity(r.ctx, 0, r.uid) })
		case 5:
			trail = append(trail, "logout")
			if some == "" {
				continue
			}
			err = r.change(raceChange{only: some}, func() error { return r.s.Logout(core.WithSession(r.ctx, some), r.uid) })
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
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	root := rootID(t, s)
	other := New(Deps{DB: s.db, Redis: redis.NewClient(&redis.Options{Addr: e.redis.Addr()}), Config: e.cfg, Authz: e.authz, Bus: e.bus, BcryptCost: bcrypt.MinCost})
	for round := range 3 {
		email := fmt.Sprintf("race-%d@example.com", round)
		u, err := s.CreateUser(ctx, root, CreateUserInput{Email: email, Password: adminPassword})
		require.NoError(t, err)
		r := &sessionRace{ctx: ctx, s: s, other: other, uid: u.ID, email: email}
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
			for _, n := range []*Service{other, s} {
				switch {
				case mustEnd:
					for _, tok := range x.tokens {
						_, _, err := n.VerifySession(ctx, tok)
						require.True(t, isCode(err, "unauthenticated"), "round %d %+v %+v %q: an ended session works: %v", round, x, r.changes, trails, err)
					}
				case !mayEnd:
					_, _, err := n.VerifySession(ctx, x.tokens[len(x.tokens)-1])
					require.NoError(t, err, "round %d %+v %+v %q: a session no change ended is rejected", round, x, r.changes, trails)
				}
			}
		}
	}
}

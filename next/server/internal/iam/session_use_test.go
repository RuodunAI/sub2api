package iam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/iam/tokenstore"
)

// How a session is used and renewed (CONTRACTS §100.1, §100.4): where it
// is used from, the new token "revoke other sessions" gives the caller,
// rotations from a token another tab's rotation left behind, and the
// requests a stale read or a failed write must not sign out.

// Revoking the other sessions also gives the caller's session a new token:
// a copy of the caller's token, which the sessions list shows as the
// caller's own session, works for the grace of a rotation only.
func TestRevokeOthersRenewsTheCallersToken(t *testing.T) {
	k := newKeyEnv(t)
	mine := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	copied := mine
	other := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	code, out := k.call(mine, "POST", "/me/sessions/revoke-others", map[string]any{})
	require.Equal(t, http.StatusOK, code, out)
	fresh, _ := out["data"].(map[string]any)["access_token"].(string)
	require.True(t, strings.HasPrefix(fresh, tokenstore.SessionPrefix) && fresh != mine, "revoke others gave %q", fresh)
	assert.Equal(t, sid(t, k.s, mine), sid(t, k.s, fresh), "the rotation changed the session id")
	ended(t, k.s, other)
	k.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	ended(t, k.s, copied)
	live(t, k.s, fresh)
}

// A rotation Valkey cannot make after the other sessions ended ends the
// caller's session too (a stampless bump), as the revocation does when it
// cannot reach Valkey: a copy of the caller's token does not live on.
func TestRevokeOthersEndsTheCallerWhenItCannotRenew(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	a := mustLogin(t, s, adminEmail, adminPassword)
	b := mustLogin(t, s, adminEmail, adminPassword)
	keep := sid(t, s, a.AccessToken)
	s.sessions = rotateFails{s.sessions}
	token, err := s.RevokeOtherSessions(ctx, a.User.ID, keep, a.AccessToken)
	require.NoError(t, err)
	assert.Empty(t, token)
	ended(t, s, a.AccessToken, b.AccessToken)
}

// A step-up from a token another tab's rotation already left in its grace
// rotates nothing: the token that rotation issued (the one the other tab
// took up, and this tab takes up from storage) is neither ended nor cut to
// the grace.
func TestStepUpFromSupersededTokenKeepsTheNewer(t *testing.T) {
	k := newKeyEnv(t)
	old := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	verify := func() map[string]any {
		t.Helper()
		code, out := k.call(old, "POST", "/me/verify", map[string]any{"scope": core.StepUpUserDelete,
			"context": "DELETE /api/v1/users/" + strconv.FormatInt(k.userID, 10), "method": "password", "password": adminPassword})
		require.Equal(t, http.StatusOK, code, out)
		return out["data"].(map[string]any)
	}
	fresh, _ := verify()["access_token"].(string)
	require.NotEmpty(t, fresh)
	second := verify()
	assert.NotEmpty(t, second["proof"])
	assert.Nil(t, second["access_token"], "a token in its grace rotated the session again")
	k.redis.FastForward(tokenstore.DefaultRotateGrace + time.Second)
	ended(t, k.s, old)
	live(t, k.s, fresh)
}

// A request records where the session is used from when its use is
// written: the sessions list shows the address and browser of the latest
// use, not only those of the login.
func TestSessionRecordsWhereItIsUsed(t *testing.T) {
	k := newKeyEnv(t)
	tok := mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	k.redis.HSet(sessKey(tok), "last_seen", strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "Elsewhere/1.0")
	req.RemoteAddr = "198.51.100.9:4321"
	w := httptest.NewRecorder()
	k.h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list, err := k.s.Sessions(context.Background(), rootID(t, k.s), "")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "198.51.100.9", list[0].IP)
	assert.Equal(t, "Elsewhere/1.0", list[0].UserAgent)
	assert.WithinDuration(t, time.Now(), list[0].LastUsedAt, time.Minute)
}

// lookupThen runs then once, after the first Lookup read the session: a
// change that lands between the read and the status check.
type lookupThen struct {
	tokenstore.SessionStore
	once *sync.Once
	then func()
}

func (l lookupThen) Lookup(ctx context.Context, token string) (tokenstore.Session, error) {
	s, err := l.SessionStore.Lookup(ctx, token)
	l.once.Do(l.then)
	return s, err
}

// A session restamped by a bump that keeps it, between the request's read
// of it and the status check (which sees the bump), is kept: the stamp is
// checked again where it would be dropped.
func TestStaleReadDoesNotDropAKeptSession(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	a := mustLogin(t, s, adminEmail, adminPassword)
	b := mustLogin(t, s, adminEmail, adminPassword)
	keep, store := sid(t, s, a.AccessToken), s.sessions
	s.sessions = lookupThen{SessionStore: store, once: &sync.Once{}, then: func() {
		_, err := s.RevokeOtherSessions(ctx, a.User.ID, keep, "")
		require.NoError(t, err)
	}}
	_, id, err := s.VerifySession(ctx, a.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, keep, id)
	s.sessions = store
	live(t, s, a.AccessToken)
	ended(t, s, b.AccessToken)
}

// A status cache that missed the user's re-enabling does not sign out a
// session issued since (stamped current): the row is read afresh, and the
// session is not dropped.
func TestStaleInactiveStatusRereadsTheRow(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, context.Background()
	p := mustLogin(t, s, adminEmail, adminPassword)
	var version int
	require.NoError(t, userTokenVersion(s.db.Gorm, p.User.ID).Row().Scan(&version))
	s.mu.Lock()
	s.status[p.User.ID] = statusEntry{active: false, tokenVersion: version, at: time.Now()}
	s.mu.Unlock()
	_, _, err := s.VerifySession(ctx, p.AccessToken)
	require.NoError(t, err)
	live(t, s, p.AccessToken)
}

// touchFails is a session store whose writes of a use fail, as Valkey out
// of memory (noeviction) refuses writes while it still answers reads.
type touchFails struct{ tokenstore.SessionStore }

func (touchFails) Touch(context.Context, string, tokenstore.Session) error {
	return tokenstore.ErrUnavailable
}

// Recording a use is not authenticating: a request whose session was read
// goes on when the write fails.
func TestFailedTouchDoesNotFailTheRequest(t *testing.T) {
	e := setupAdmin(t)
	s := e.svc
	p := mustLogin(t, s, adminEmail, adminPassword)
	s.sessions = touchFails{s.sessions}
	live(t, s, p.AccessToken)
}

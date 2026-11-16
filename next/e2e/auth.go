package e2e

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// Session is a logged-in console user.
type Session struct {
	*Client
	Email    string
	Password string
	UserID   int64
}

// Login performs POST /auth/login and returns a session. Fails the test on
// any non-200 answer.
func (e *Env) Login(email, password string) *Session {
	e.T.Helper()
	c := NewClient(e.BaseURL)
	r := c.API(e.T, http.MethodPost, "/auth/login", map[string]any{"email": email, "password": password})
	if r.Status != 200 {
		e.T.Fatalf("login %s: %s", email, r)
	}
	d := r.Data()
	// A console session token (CONTRACTS §100), and no refresh token.
	if !strings.HasPrefix(d.Get("access_token").String(), "s2s_") || d.Get("refresh_token").Exists() || d.Get("expires_in").Int() <= 0 {
		e.T.Fatalf("login response not a session: %s", r)
	}
	c.Token = d.Get("access_token").String()
	c.Password = password
	return &Session{Client: c, Email: email, Password: password, UserID: d.Get("user.id").Int()}
}

// TryLogin returns the raw login response (for negative tests).
func (e *Env) TryLogin(email, password string) *Resp {
	e.T.Helper()
	return NewClient(e.BaseURL).API(e.T, http.MethodPost, "/auth/login", map[string]any{"email": email, "password": password})
}

var (
	adminMu   sync.Mutex
	adminSess *Session
	adminAt   time.Time
)

// Admin returns a cached super-admin session (re-login every 30 minutes).
func (e *Env) Admin() *Session {
	e.T.Helper()
	e.RequireAdmin()
	adminMu.Lock()
	defer adminMu.Unlock()
	if adminSess == nil || time.Since(adminAt) > 30*time.Minute {
		adminSess = e.Login(e.AdminEmail, e.AdminPassword)
		adminAt = time.Now()
	}
	return adminSess
}

// Me returns GET /me.
func (s *Session) Me(t testing.TB) gjson.Result {
	t.Helper()
	return s.OK(t, http.MethodGet, "/me", nil)
}

// Permissions returns the permission keys of GET /me as a set.
func (s *Session) Permissions(t testing.TB) map[string]bool {
	t.Helper()
	return Set(s.Me(t).Get("permissions"))
}

// Menus returns GET /me/menus flattened to items (each with its section).
func (s *Session) Menus(t testing.TB) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	for _, sec := range s.OK(t, http.MethodGet, "/me/menus", nil).Array() {
		out = append(out, sec.Get("items").Array()...)
	}
	return out
}

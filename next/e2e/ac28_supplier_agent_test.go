package e2e

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// AC 28: supplier agents and team scopes (CONTRACTS §102, plan 0.1.291
// §5.1–5.6). The tree is admin → agent A → agent B → supplier C, beside an
// unrelated agent X and a plain user U. Every check is a way out of the
// team: reading or changing users, accounts, usage and plugin routes past
// it, naming owners, assigning roles, narrowing access keys, and what a
// deleted user or a moved account changes.

// agentTree is the fixture of AC 28.
type agentTree struct {
	A, B, C, X, U  *Session
	accA, accB     int64 // accounts A and B made themselves
	accC, accX     int64
	proxyC         int64 // made with accC from its proxy_url
	served, failed int64 // tenant accounts: served owned by C, failed by X
	record         int64 // usage record of a request failed over from failed to served
}

func agentRoles(extra ...string) []string { return append([]string{"supplier_agent"}, extra...) }

// newAgentTree builds the tree and its accounts; cl collects what to delete.
func newAgentTree(e *Env, admin *Session, cl *OwnershipCleanup) *agentTree {
	t := e.T
	t.Helper()
	tr := &agentTree{}
	user := func(creator *Session, spec UserSpec) *Session {
		s := e.CreateUser(creator, spec)
		cl.Users = append(cl.Users, s.UserID)
		return s
	}
	tr.A = user(admin, UserSpec{Roles: agentRoles()})
	tr.X = user(admin, UserSpec{Roles: agentRoles()})
	tr.U = user(admin, UserSpec{})
	// An agent's users are its own without naming the owner.
	tr.B = user(tr.A, UserSpec{Roles: agentRoles()})
	tr.C = user(tr.B, UserSpec{Roles: []string{"user", "supplier"}})
	for _, c := range []struct{ child, owner *Session }{{tr.B, tr.A}, {tr.C, tr.B}} {
		u := admin.OK(t, http.MethodGet, fmt.Sprintf("/users/%d", c.child.UserID), nil)
		if u.Get("owner_id").Int() != c.owner.UserID || u.Get("owner_email").String() != c.owner.Email {
			t.Fatalf("user %s: want owner %s, got %s", c.child.Email, c.owner.Email, u.Raw)
		}
	}

	account := func(s *Session, extra map[string]any) gjson.Result {
		d := e.CreateOwnedAccount(s, e.OwnedAccountBody("", extra))
		cl.Accounts = append(cl.Accounts, d.Get("id").Int())
		if d.Get("owner_id").Int() != s.UserID || d.Get("created_by").Int() != s.UserID {
			t.Fatalf("account of %s: owner and creator should be its maker: %s", s.Email, d.Raw)
		}
		return d
	}
	tr.accA = account(tr.A, nil).Get("id").Int()
	tr.accB = account(tr.B, nil).Get("id").Int()
	c := account(tr.C, map[string]any{"proxy_url": "socks5://u:p@10.0.28.1:1080"})
	tr.accC, tr.proxyC = c.Get("id").Int(), c.Get("proxy_id").Int()
	cl.Proxies = append(cl.Proxies, tr.proxyC)
	tr.accX = account(tr.X, nil).Get("id").Int()

	// Traffic: the admin's accounts, then given to C and X. The request
	// fails on X's account and is served by C's.
	tn := e.NewTenant(admin, TenantOpts{Accounts: 2, Priorities: []int{50, 1}})
	failed, served := tn.Accounts[0], tn.Accounts[1]
	tr.failed, tr.served = failed.ID, served.ID
	cl.Accounts = append(cl.Accounts, failed.ID, served.ID)
	ExpectNotFound(t, tr.A.API(t, http.MethodGet, fmt.Sprintf("/accounts/%d", served.ID), nil))
	moveAccount(e, admin, served.ID, tr.C)
	moveAccount(e, admin, failed.ID, tr.X)

	m := e.Mock()
	m.SetRule(t, MockRule{APIKey: failed.Key, Status: 429, Remaining: 1})
	defer m.ClearRule(t, failed.Key)
	g := e.MustMessages(tn.APIKey, MessagesBody(tn.Model, "agent tree", false), nil)
	rec := e.UsageByRequest(admin, tn.User.UserID, g.RequestID)
	if rec.Get("account_id").Int() != served.ID || len(rec.Get("attempt_trail.steps").Array()) != 2 {
		t.Fatalf("expected a failover from %d to %d: %s", failed.ID, served.ID, rec.Raw)
	}
	tr.record = rec.Get("id").Int()
	return tr
}

// moveAccount gives the account to owner (PUT /accounts/:id/owner, admin).
func moveAccount(e *Env, admin *Session, id int64, owner *Session) {
	e.T.Helper()
	d := admin.OK(e.T, http.MethodPut, fmt.Sprintf("/accounts/%d/owner", id), map[string]any{"owner_id": owner.UserID})
	if d.Get("owner_id").Int() != owner.UserID || d.Get("owner_email").String() != owner.Email {
		e.T.Fatalf("PUT /accounts/%d/owner: %s", id, d.Raw)
	}
}

// userIDs is the ids GET /users lists to s with the query kv.
func userIDs(t *testing.T, s *Session, kv ...string) []int64 {
	t.Helper()
	var ids []int64
	for _, u := range s.ListAll(t, "/users", kv...) {
		ids = append(ids, u.Get("id").Int())
	}
	slices.Sort(ids)
	return ids
}

// accountIDs is the ids GET /accounts lists to s with the query kv.
func accountIDs(t *testing.T, s *Session, kv ...string) map[int64]bool {
	t.Helper()
	return IDSet(s.ListAll(t, "/accounts", kv...))
}

func sorted(ids ...int64) []int64 {
	slices.Sort(ids)
	return ids
}

func TestAC28_SupplierAgentTeam(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	e.EnsurePlugin(admin, "anthropic", "")
	cl := &OwnershipCleanup{}
	defer cl.Run(e, admin)
	tr := newAgentTree(e, admin, cl)
	A, B, C, X, U := tr.A, tr.B, tr.C, tr.X, tr.U
	userPath := func(s *Session) string { return fmt.Sprintf("/users/%d", s.UserID) }
	accPath := func(id int64, rest string) string { return fmt.Sprintf("/accounts/%d%s", id, rest) }

	// §5.1: users out of the team are not found; the team is everyone
	// below, never the holder itself.
	t.Run("users", func(t *testing.T) {
		if got := userIDs(t, A); !slices.Equal(got, sorted(B.UserID, C.UserID)) {
			t.Fatalf("A lists %v, want B and C", got)
		}
		if got := userIDs(t, B); !slices.Equal(got, []int64{C.UserID}) {
			t.Fatalf("B lists %v, want C", got)
		}
		if got := userIDs(t, A, "direct", "true"); !slices.Equal(got, []int64{B.UserID}) {
			t.Fatalf("A direct lists %v, want B", got)
		}
		if got := userIDs(t, A, "owner_id", itoa(B.UserID)); !slices.Equal(got, []int64{C.UserID}) {
			t.Fatalf("A owner_id=B lists %v", got)
		}
		for _, kv := range [][]string{{"owner_id", itoa(X.UserID)}, {"owner_id", "none"}, {"q", X.Email}, {"q", A.Email}} {
			if got := userIDs(t, A, kv...); len(got) != 0 {
				t.Errorf("A lists %v with %v", got, kv)
			}
		}
		ExpectFieldError(t, A.API(t, http.MethodGet, "/users", nil, Query("owner_id", "-1")), "owner_id", "invalid")

		// Found in the team; others, the agent itself and its owner not.
		A.OK(t, http.MethodGet, userPath(C), nil)
		for _, c := range []struct {
			s      *Session
			target *Session
		}{{A, X}, {A, A}, {A, U}, {B, A}, {B, B}, {X, B}, {X, C}} {
			ExpectNotFound(t, c.s.API(t, http.MethodGet, userPath(c.target), nil))
			ExpectNotFound(t, c.s.API(t, http.MethodPatch, userPath(c.target), map[string]any{"display_name": "x"}))
		}
		ExpectNotFound(t, A.API(t, http.MethodGet, "/users/1", nil))
		// D5: no password reset, no deletion, even in the team.
		ExpectPermissionDenied(t, A.API(t, http.MethodPatch, userPath(C), map[string]any{"password": "E2e-" + e.RunID + "-new!"}))
		ExpectPermissionDenied(t, A.API(t, http.MethodDelete, userPath(C), nil))

		// Lookup: the same scope.
		look := func(s *Session, kv ...string) map[int64]bool {
			return IDSet(s.OK(t, http.MethodGet, "/lookup/users", nil, Query(kv...)).Get("items").Array())
		}
		if got := look(A, "ids", fmt.Sprintf("%d,%d,%d", B.UserID, X.UserID, A.UserID)); len(got) != 1 || !got[B.UserID] {
			t.Errorf("A looks up %v by ids, want B only", got)
		}
		if got := look(A, "q", X.Email); len(got) != 0 {
			t.Errorf("A finds X by email: %v", got)
		}
		if got := look(B, "q", "@e2e.test", "limit", "50"); len(got) != 1 || !got[C.UserID] {
			t.Errorf("B looks up %v, want C only", got)
		}

		// Unscoped user:read uses refuse a team holder, without a 500.
		for _, p := range []string{"/usage", "/usage/overview", "/segments/1/members"} {
			ExpectPermissionDenied(t, A.API(t, http.MethodGet, p, nil))
		}
		ExpectPermissionDenied(t, A.API(t, http.MethodPost, "/pricing/quote", map[string]any{"user_id": C.UserID}))
	})

	// §5.1: subordinates' accounts are read-only, the rest are not found;
	// subordinates' proxies are not found.
	t.Run("accounts", func(t *testing.T) {
		e := e.With(t)
		got := accountIDs(t, A)
		for _, id := range []int64{tr.accA, tr.accB, tr.accC, tr.served} {
			if !got[id] {
				t.Errorf("A's accounts lack %d", id)
			}
		}
		for _, id := range []int64{tr.accX, tr.failed} {
			if got[id] {
				t.Errorf("A's accounts include X's %d", id)
			}
			ExpectNotFound(t, A.API(t, http.MethodGet, accPath(id, ""), nil))
		}
		if got := accountIDs(t, A, "subordinates", "true"); len(got) != 3 || !got[tr.accB] || !got[tr.accC] || !got[tr.served] {
			t.Errorf("A subordinates: %v", got)
		}
		if got := accountIDs(t, A, "mine", "true"); len(got) != 1 || !got[tr.accA] {
			t.Errorf("A mine: %v", got)
		}
		if got := accountIDs(t, A, "owner_id", itoa(X.UserID)); len(got) != 0 {
			t.Errorf("A filters X's accounts: %v", got)
		}
		looked := IDSet(A.OK(t, http.MethodGet, "/lookup/accounts", nil, Query("ids", fmt.Sprintf("%d,%d", tr.accC, tr.accX))).Get("items").Array())
		if len(looked) != 1 || !looked[tr.accC] {
			t.Errorf("A looks up accounts %v", looked)
		}
		if got := accountIDs(t, C, "subordinates", "true"); len(got) != 0 {
			t.Errorf("a supplier has no subordinates: %v", got)
		}
		if got := accountIDs(t, B); got[tr.accA] || !got[tr.accC] {
			t.Errorf("B sees its owner's or misses its subordinate's account: %v", got)
		}

		d := A.OK(t, http.MethodGet, accPath(tr.accC, ""), nil)
		if k := d.Get("credentials.api_key").String(); k != "" && k != "******" {
			t.Fatalf("A reads C's api_key: %s", d.Raw)
		}
		if d.Get("owner_id").Int() != C.UserID {
			t.Errorf("C's account owner: %s", d.Raw)
		}
		A.OK(t, http.MethodGet, accPath(tr.accC, "/quota"), nil)
		// Balance is past the scope check: the type has none.
		if r := A.API(t, http.MethodGet, accPath(tr.accC, "/balance"), nil); r.ErrCode() != "unsupported" {
			t.Errorf("A reads C's balance: %s", r)
		}
		ExpectNotFound(t, X.API(t, http.MethodGet, accPath(tr.accC, "/balance"), nil))
		ExpectNotFound(t, X.API(t, http.MethodGet, accPath(tr.accC, "/quota"), nil))
		A.OK(t, http.MethodGet, accPath(tr.accC, "/usage"), nil)
		for _, c := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPatch, accPath(tr.accC, ""), map[string]any{"name": e.Name("x")}},
			{http.MethodDelete, accPath(tr.accC, ""), nil},
			{http.MethodPost, accPath(tr.accC, "/test"), map[string]any{}},
			{http.MethodPost, accPath(tr.accC, "/credentials/reveal"), map[string]any{}},
			{http.MethodPost, accPath(tr.accC, "/reset-status"), map[string]any{}},
			{http.MethodGet, fmt.Sprintf("/proxies/%d", tr.proxyC), nil},
			{http.MethodPatch, fmt.Sprintf("/proxies/%d", tr.proxyC), map[string]any{"name": e.Name("x")}},
		} {
			ExpectNotFound(t, A.API(t, c.method, c.path, c.body))
		}
		if IDSet(A.ListAll(t, "/proxies"))[tr.proxyC] {
			t.Error("A lists C's proxy")
		}
		// Moving an account is the admin's alone.
		ExpectPermissionDenied(t, A.API(t, http.MethodPut, accPath(tr.accC, "/owner"), map[string]any{"owner_id": A.UserID}))
	})

	// §5.1, D1: the account-side usage of the team's accounts, whitelisted.
	t.Run("account usage", func(t *testing.T) {
		recPath := fmt.Sprintf("/account-usage/%d", tr.record)
		for _, s := range []*Session{A, B, C} {
			d := s.OK(t, http.MethodGet, recPath, nil)
			if d.Get("account_id").Int() != tr.served {
				t.Fatalf("%s: record %s", s.Email, d.Raw)
			}
			for _, f := range []string{"user_id", "user_email", "api_key_id", "client_ip", "group_id", "total_cost", "billing_detail", "error_message"} {
				if d.Get(f).Exists() {
					t.Errorf("%s: record has %s: %s", s.Email, f, d.Raw)
				}
			}
			// X's account in the chain is a reference only.
			steps := d.Get("attempt_trail.steps").Array()
			if len(steps) != 2 || steps[0].Get("account_id").Exists() || steps[0].Get("account_name").Exists() ||
				!steps[0].Get("account_ref").Exists() || steps[1].Get("account_id").Int() != tr.served || steps[0].Get("message").Exists() {
				t.Errorf("%s: trail %s", s.Email, d.Get("attempt_trail").Raw)
			}
		}
		ExpectNotFound(t, X.API(t, http.MethodGet, recPath, nil))
		if items := X.ListAll(t, "/account-usage", "account_id", itoa(tr.failed)); len(items) != 0 {
			t.Errorf("X sees a record its account only failed: %v", items)
		}
		if items := A.ListAll(t, "/account-usage", "account_id", itoa(tr.failed)); len(items) != 0 {
			t.Errorf("A filters by X's account: %v", items)
		}
		if _, ok := Find(A.ListAll(t, "/account-usage"), "id", tr.record); !ok {
			t.Error("A's account usage lacks the record")
		}
		for _, kv := range [][]string{{"user_id", itoa(U.UserID)}, {"api_key_id", "1"}, {"client_ip", "127.0.0.1"}, {"group_id", "1"}, {"user_email", U.Email}} {
			if code := FieldError(A.API(t, http.MethodGet, "/account-usage", nil, Query(kv...)), kv[0]); code != "not_allowed" {
				t.Errorf("filter %s: field error %q", kv[0], code)
			}
		}
		for _, by := range []string{"user", "api_key", "group", "ip"} {
			r := A.API(t, http.MethodGet, "/account-usage/summary", nil, Query("group_by", by))
			if r.Status != 400 {
				t.Errorf("summary group_by=%s: %s", by, r)
			}
		}
		r := A.API(t, http.MethodGet, "/account-usage/export", nil, Query("lang", "en"))
		if r.Status != 200 {
			t.Fatalf("export: %s", r)
		}
		header := strings.SplitN(strings.TrimPrefix(string(r.Body), "\ufeff"), "\n", 2)[0]
		for _, bad := range []string{"User", "API key", "IP", "Group", "Cost"} {
			if strings.Contains(header, bad) {
				t.Errorf("export header has %q: %s", bad, header)
			}
		}
		// Overviews are per scope: X, asked right after A, gets its own.
		ov := func(s *Session) gjson.Result { return s.OK(t, http.MethodGet, "/account-usage/overview", nil) }
		a, x := ov(A), ov(X)
		if a.Get("accounts.total").Int() != 4 || x.Get("accounts.total").Int() != 2 {
			t.Errorf("overview accounts: A %s, X %s", a.Get("accounts").Raw, x.Get("accounts").Raw)
		}
		if a.Get("today.requests").Int() < 1 || x.Get("today.requests").Int() != 0 || x.Get("users").Exists() || x.Get("today.total_cost").Exists() {
			t.Errorf("overview: A %s, X %s", a.Raw, x.Raw)
		}
	})

	// §5.6: plugin account routes take the same team, read-only.
	t.Run("plugin account routes", func(t *testing.T) {
		e := e.With(t)
		e.EnsurePlugin(admin, "dynweight", "")
		roleID := e.CreateRole(admin, e.RoleKey("dw"), "dynweight", []string{"plugin.dynweight:dynweight:read", "plugin.dynweight:dynweight:manage"})
		cl.Roles = append(cl.Roles, roleID)
		role := admin.OK(t, http.MethodGet, fmt.Sprintf("/roles/%d", roleID), nil).Get("key").String()
		admin.OK(t, http.MethodPut, userPath(A)+"/roles", map[string]any{"role_keys": agentRoles(role)})
		defer admin.OK(t, http.MethodPut, userPath(A)+"/roles", map[string]any{"role_keys": agentRoles()})
		route := func(method, path string, body any) *Resp {
			return e.PluginRoute(A, e.BaseURL, method, "dynweight", path, body)
		}
		for _, id := range []int64{tr.accA, tr.accC} {
			if r := route(http.MethodGet, "/status?account_id="+itoa(id), nil); r.Status != 200 {
				t.Errorf("status of %d: %s", id, r)
			}
		}
		ExpectNotFound(t, route(http.MethodGet, "/status?account_id="+itoa(tr.accX), nil))
		ExpectNotFound(t, route(http.MethodPost, "/reset", map[string]any{"account_id": tr.accC}))
		ExpectNotFound(t, route(http.MethodPost, "/reset", map[string]any{"account_ids": []int64{tr.accA, tr.accC}}))
		if r := route(http.MethodPost, "/reset", map[string]any{"account_id": tr.accA}); r.Status != 200 {
			t.Errorf("reset of A's own account: %s", r)
		}
		// Routes naming no account need every account.
		ExpectPermissionDenied(t, route(http.MethodGet, "/stats", nil))
	})

	// §5.5: an access key narrows before the team is expanded.
	t.Run("access keys", func(t *testing.T) {
		e := e.With(t)
		key := func(s *Session, perms ...string) *Client {
			d := s.OK(t, http.MethodPost, "/me/access-keys", map[string]any{"name": e.Name("ak"), "permissions": perms})
			if !strings.HasPrefix(d.Get("key").String(), "sak_") {
				t.Fatalf("access key: %s", d.Raw)
			}
			return &Client{Base: e.BaseURL, HTTP: s.HTTP, Token: d.Get("key").String()}
		}
		// A key can hold no more than its holder.
		ExpectPermissionDenied(t, A.API(t, http.MethodPost, "/me/access-keys", map[string]any{"name": e.Name("ak"), "permissions": []string{"user:read"}}))

		own := key(A, "account:read:own", "user:read:team")
		if got := accountIDs(t, &Session{Client: own}); len(got) != 1 || !got[tr.accA] {
			t.Errorf("key account:read:own lists %v", got)
		}
		ExpectNotFound(t, own.API(t, http.MethodGet, accPath(tr.accC, ""), nil))
		if got := userIDs(t, &Session{Client: own}); !slices.Equal(got, sorted(B.UserID, C.UserID)) {
			t.Errorf("key user:read:team lists %v", got)
		}
		team := key(A, "account:read:team")
		team.OK(t, http.MethodGet, accPath(tr.accC, ""), nil)
		ExpectPermissionDenied(t, team.API(t, http.MethodGet, "/users", nil))

		// X's key keeps account:read:team after X becomes a plain supplier:
		// it reaches X's own accounts only, and the users not at all.
		xk := key(X, "account:read:team", "user:read:team")
		X.OK(t, http.MethodGet, "/me", nil)
		admin.OK(t, http.MethodPut, userPath(X)+"/roles", map[string]any{"role_keys": []string{"user", "supplier"}})
		defer admin.OK(t, http.MethodPut, userPath(X)+"/roles", map[string]any{"role_keys": agentRoles()})
		if got := accountIDs(t, &Session{Client: xk}); !got[tr.accX] || got[tr.accA] {
			t.Errorf("X's key after the downgrade lists %v", got)
		}
		ExpectPermissionDenied(t, xk.API(t, http.MethodGet, "/users", nil))
	})

	// §5.2, D2, D6: roles are assigned within the team and the assignable
	// lists only.
	t.Run("role assignment", func(t *testing.T) {
		e := e.With(t)
		roles := func(s, target *Session, keys ...string) *Resp {
			return s.API(t, http.MethodPut, userPath(target)+"/roles", map[string]any{"role_keys": keys})
		}
		for _, keys := range [][]string{{"user", "admin"}, {"user", "operator"}, {"super_admin"}} {
			ExpectPermissionDenied(t, roles(A, C, keys...))
			ExpectPermissionDenied(t, A.API(t, http.MethodPost, "/users", map[string]any{
				"email": e.Name("esc") + "@e2e.test", "display_name": "x", "password": "E2e-" + e.RunID + "-pass!", "role_keys": keys,
			}))
		}
		for _, c := range []struct{ s, target *Session }{{A, X}, {A, U}, {A, A}, {B, A}, {X, C}} {
			ExpectPermissionDenied(t, roles(c.s, c.target, "user", "supplier"))
		}
		// The roles a user has already: no change, but no read of the
		// user past the team either.
		for _, c := range []struct{ s, target *Session }{{B, A}, {X, B}, {A, U}} {
			current := Strings(admin.OK(t, http.MethodGet, userPath(c.target), nil).Get("roles"))
			if r := roles(c.s, c.target, current...); r.Status != 403 || r.JSON().Get("error.details.reason").String() != "not_in_team" {
				t.Errorf("%s re-assigns %s's roles: %s", c.s.Email, c.target.Email, r)
			}
		}
		// Multi-level (D2): A makes C an agent; C's team is below B's.
		A.OK(t, http.MethodPut, userPath(C)+"/roles", map[string]any{"role_keys": []string{"user", "supplier", "supplier_agent"}})
		D := e.CreateUser(C, UserSpec{Roles: []string{"user", "supplier"}})
		cl.Users = append(cl.Users, D.UserID)
		for _, s := range []*Session{A, B, C} {
			s.OK(t, http.MethodGet, userPath(D), nil)
		}
		if got := userIDs(t, A); !slices.Equal(got, sorted(B.UserID, C.UserID, D.UserID)) {
			t.Errorf("A's team after D: %v", got)
		}
		ExpectNotFound(t, C.API(t, http.MethodGet, userPath(B), nil))
		ExpectPermissionDenied(t, roles(C, B, "user", "supplier"))
		A.OK(t, http.MethodPut, userPath(C)+"/roles", map[string]any{"role_keys": []string{"user", "supplier"}})

		// Owner invariant (§5.4): C's owner B cannot manage an operator.
		ExpectFieldError(t, roles(admin, C, "user", "supplier", "operator"), "owner_id", "owner_cannot_manage")
		ExpectFieldError(t, roles(admin, C, "user", "admin"), "owner_id", "owner_cannot_manage")

		// D6: an operator neither assigns supplier_agent nor manages agents.
		O := e.CreateUser(admin, UserSpec{Roles: []string{"user", "operator"}})
		cl.Users = append(cl.Users, O.UserID)
		ExpectPermissionDenied(t, roles(O, U, "user", "supplier_agent"))
		ExpectPermissionDenied(t, O.API(t, http.MethodPost, "/users", map[string]any{
			"email": e.Name("op") + "@e2e.test", "display_name": "x", "password": "E2e-" + e.RunID + "-pass!", "role_keys": agentRoles(),
		}))
		ExpectPermissionDenied(t, roles(O, A, "user", "supplier"))
		ExpectPermissionDenied(t, O.API(t, http.MethodPatch, userPath(A), map[string]any{"display_name": "x"}))
		ExpectPermissionDenied(t, O.API(t, http.MethodPatch, userPath(U), map[string]any{"owner_id": A.UserID}))
		O.OK(t, http.MethodPut, userPath(U)+"/roles", map[string]any{"role_keys": []string{"user", "supplier"}})
		O.OK(t, http.MethodPut, userPath(U)+"/roles", map[string]any{"role_keys": []string{"user"}})
		// An operator is not made anyone's subordinate.
		ExpectFieldError(t, admin.API(t, http.MethodPatch, userPath(O), map[string]any{"owner_id": A.UserID}), "owner_id", "owner_cannot_manage")
	})

	// §5.3: owners stay within the team, without cycles, 8 levels deep.
	t.Run("owners", func(t *testing.T) {
		e := e.With(t)
		patch := func(s, target *Session, owner int64) *Resp {
			return s.API(t, http.MethodPatch, userPath(target), map[string]any{"owner_id": owner})
		}
		for _, owner := range []int64{X.UserID, C.UserID, U.UserID, 0} {
			ExpectPermissionDenied(t, patch(A, B, owner))
		}
		ExpectPermissionDenied(t, A.API(t, http.MethodPost, "/users", map[string]any{
			"email": e.Name("own") + "@e2e.test", "display_name": "x", "password": "E2e-" + e.RunID + "-pass!", "owner_id": X.UserID,
		}))
		A.OK(t, http.MethodPatch, userPath(B), map[string]any{"owner_id": A.UserID}) // unchanged
		ExpectFieldError(t, patch(admin, A, A.UserID), "owner_id", "self")
		ExpectFieldError(t, patch(admin, A, B.UserID), "owner_id", "cycle")
		ExpectFieldError(t, patch(admin, A, C.UserID), "owner_id", "cycle")
		ExpectFieldError(t, patch(admin, U, 999_999_999), "owner_id", "not_found")

		// Two moves that would close a cycle together: one wins.
		P := e.CreateUser(admin, UserSpec{})
		Q := e.CreateUser(admin, UserSpec{})
		cl.Users = append(cl.Users, P.UserID, Q.UserID)
		var wg sync.WaitGroup
		res := make([]*Resp, 2)
		for i, c := range [][2]*Session{{P, Q}, {Q, P}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = admin.API(t, http.MethodPatch, userPath(c[0]), map[string]any{"owner_id": c[1].UserID})
			}()
		}
		wg.Wait()
		if ok := (res[0].Status == 200) != (res[1].Status == 200); !ok || FieldError(res[0], "owner_id")+FieldError(res[1], "owner_id") != "cycle" {
			t.Errorf("concurrent cycle: %s | %s", res[0], res[1])
		}

		// A chain of agents 8 levels below its top: nobody goes below it.
		chain := []*Session{e.CreateUser(admin, UserSpec{Roles: agentRoles()})}
		for i := 1; i <= 8; i++ {
			owner := chain[i-1].UserID
			chain = append(chain, e.CreateUser(admin, UserSpec{Roles: agentRoles(), OwnerID: &owner}))
		}
		for _, s := range chain {
			cl.Users = append(cl.Users, s.UserID)
		}
		last := chain[8]
		ExpectFieldError(t, last.API(t, http.MethodPost, "/users", map[string]any{
			"email": e.Name("deep") + "@e2e.test", "display_name": "x", "password": "E2e-" + e.RunID + "-pass!",
		}), "owner_id", "too_deep")
		ExpectFieldError(t, admin.API(t, http.MethodPost, "/users", map[string]any{
			"email": e.Name("deep") + "@e2e.test", "display_name": "x", "password": "E2e-" + e.RunID + "-pass!", "owner_id": last.UserID,
		}), "owner_id", "too_deep")
		// Seven levels hang under A (depth 0) but not under B (depth 1).
		ExpectFieldError(t, patch(admin, chain[1], B.UserID), "owner_id", "too_deep")
		if got := userIDs(t, chain[0]); len(got) != 8 {
			t.Errorf("the top of the chain lists %d users, want 8", len(got))
		}

		// A team holder may take a user from below into its own hands.
		E := e.CreateUser(B, UserSpec{Roles: []string{"user", "supplier"}})
		cl.Users = append(cl.Users, E.UserID)
		if d := A.OK(t, http.MethodPatch, userPath(E), map[string]any{"owner_id": A.UserID}); d.Get("owner_id").Int() != A.UserID {
			t.Errorf("A takes E: %s", d.Raw)
		}
		ExpectNotFound(t, B.API(t, http.MethodGet, userPath(E), nil))
		// Moved away by the admin, E leaves A's team at once.
		admin.OK(t, http.MethodPatch, userPath(E), map[string]any{"owner_id": X.UserID})
		ExpectNotFound(t, A.API(t, http.MethodGet, userPath(E), nil))
		X.OK(t, http.MethodGet, userPath(E), nil)
	})

	// D7 and §102.4: a deleted user stays in the team; moving an account
	// moves who sees it at once.
	t.Run("deleted users and moved accounts", func(t *testing.T) {
		e := e.With(t)
		admin.OK(t, http.MethodDelete, userPath(B), nil)
		if got := userIDs(t, A); slices.Contains(got, B.UserID) || !slices.Contains(got, C.UserID) {
			t.Errorf("A after B's deletion lists %v", got)
		}
		ExpectNotFound(t, A.API(t, http.MethodGet, userPath(B), nil))
		c := A.OK(t, http.MethodGet, userPath(C), nil)
		if c.Get("owner_id").Int() != B.UserID || c.Get("owner_email").String() != B.Email {
			t.Errorf("C's owner after B's deletion: %s", c.Raw)
		}
		got := accountIDs(t, A)
		if !got[tr.accB] || !got[tr.accC] {
			t.Errorf("A loses the accounts below a deleted user: %v", got)
		}
		ExpectFieldError(t, admin.API(t, http.MethodPatch, userPath(U), map[string]any{"owner_id": B.UserID}), "owner_id", "not_found")
		ExpectFieldError(t, admin.API(t, http.MethodPut, accPath(tr.accA, "/owner"), map[string]any{"owner_id": B.UserID}), "owner_id", "not_found")

		// The admin hangs C under A, away from the deleted B.
		if d := admin.OK(t, http.MethodPatch, userPath(C), map[string]any{"owner_id": A.UserID}); d.Get("owner_id").Int() != A.UserID {
			t.Errorf("C moved to A: %s", d.Raw)
		}
		admin.OK(t, http.MethodDelete, userPath(C), nil)
		if got := accountIDs(t, A); !got[tr.accC] || !got[tr.served] {
			t.Errorf("A loses a deleted subordinate's accounts: %v", got)
		}
		ExpectNotFound(t, A.API(t, http.MethodGet, userPath(C), nil))

		// The served account goes to X: A no longer sees it or its usage.
		moveAccount(e, admin, tr.served, X)
		ExpectNotFound(t, A.API(t, http.MethodGet, accPath(tr.served, ""), nil))
		ExpectNotFound(t, A.API(t, http.MethodGet, fmt.Sprintf("/account-usage/%d", tr.record), nil))
		X.OK(t, http.MethodGet, accPath(tr.served, ""), nil)
		X.OK(t, http.MethodGet, fmt.Sprintf("/account-usage/%d", tr.record), nil)
		if d := admin.OK(t, http.MethodGet, accPath(tr.served, ""), nil); d.Get("created_by").Int() == X.UserID {
			t.Errorf("moving an account changed its creator: %s", d.Raw)
		}
	})
}

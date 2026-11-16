package account

import (
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// 所属 (CONTRACTS §103): a new account is owned by its creator; own scopes,
// mine and owner_id follow the owner, created_by the creator. Only
// account:owner:update moves the owner (PUT /accounts/:id/owner), to a user
// that is not deleted, audited as account.owner; the account goes with it.
func TestAccountOwner(t *testing.T) {
	az := fakeAuthz{keys: map[int64][]string{}}
	e := setupWith(t, az)
	admin := e.uid
	alice, bob, gone := e.addUser("alice@x.com"), e.addUser("bob@x.com"), e.addUser("gone@x.com")
	az.keys[admin] = append(slices.Clone(adminKeys), "account:owner:update")
	az.keys[alice], az.keys[bob] = supplierKeys, supplierKeys
	e.exec1(`UPDATE users SET deleted_at = now() WHERE id = $1 RETURNING id`, gone)

	a := e.createAs(alice, "alice's", nil)
	id := int64(a["id"].(float64))
	if a["owner_id"] != float64(alice) || a["owner_email"] != "alice@x.com" {
		t.Fatalf("new account: %v", a)
	}
	path := fmt.Sprintf("/accounts/%d/owner", id)
	if code, out := e.doAs(alice, "PUT", path, map[string]any{"owner_id": bob}); code != 403 {
		t.Fatalf("owner moved without account:owner:update: %d %v", code, out)
	}
	for _, to := range []int64{gone, 999999} {
		if code, out := e.do("PUT", path, map[string]any{"owner_id": to}); code != 400 || fmt.Sprint(fieldsOf(out)) != "[owner_id:not_found]" {
			t.Fatalf("owner %d: %d %v", to, code, out)
		}
	}
	if code, out := e.do("PUT", "/accounts/999999/owner", map[string]any{"owner_id": bob}); code != 404 {
		t.Fatalf("missing account: %d %v", code, out)
	}
	for range 2 { // the second names the owner it has: nothing to audit
		code, out := e.do("PUT", path, map[string]any{"owner_id": bob})
		if d := data(out); code != 200 || d["id"] != float64(id) || d["owner_id"] != float64(bob) || d["owner_email"] != "bob@x.com" {
			t.Fatalf("move: %d %v", code, out)
		}
	}
	rows := e.auditRows("account.owner")
	if len(rows) != 1 || rows[0]["user_id"] != admin || rows[0]["target_id"] != strconv.FormatInt(id, 10) ||
		fmt.Sprint(rows[0]["detail"]) != fmt.Sprintf("map[from:%d to:%d]", alice, bob) {
		t.Fatalf("audit: %v", rows)
	}

	// Bob's now, alice's no more; its creator stays alice.
	if _, out := e.doAs(bob, "GET", "/accounts", nil); listIDs(out) != strconv.FormatInt(id, 10) {
		t.Fatalf("bob list: %v", out)
	}
	if _, out := e.doAs(alice, "GET", "/accounts", nil); total(out) != 0 {
		t.Fatalf("alice list: %v", out)
	}
	if code, out := e.doAs(bob, "GET", fmt.Sprintf("/accounts/%d", id), nil); code != 200 || data(out)["created_by"] != float64(alice) || data(out)["owner_id"] != float64(bob) {
		t.Fatalf("bob get: %d %v", code, out)
	}
	for q, want := range map[string]float64{
		"mine=true": 0, fmt.Sprintf("owner_id=%d", bob): 1, fmt.Sprintf("owner_id=%d", alice): 0, fmt.Sprintf("created_by=%d", alice): 1,
	} {
		if _, out := e.do("GET", "/accounts?"+q, nil); total(out) != want {
			t.Fatalf("%s: %v", q, out)
		}
	}
	if _, out := e.doAs(bob, "GET", "/accounts?mine=true", nil); total(out) != 1 {
		t.Fatalf("bob mine: %v", out)
	}
	if code, out := e.do("GET", "/accounts?owner_id=abc", nil); code != 400 || fmt.Sprint(fieldsOf(out)) != "[owner_id:invalid]" {
		t.Fatalf("bad owner_id: %d %v", code, out)
	}

	// The administrator's lock follows the owner too: disabling an account
	// it owns, whoever created it, does not lock it.
	if code, out := e.do("PUT", path, map[string]any{"owner_id": admin}); code != 200 {
		t.Fatalf("move to admin: %d %v", code, out)
	}
	if code, out := e.do("PATCH", fmt.Sprintf("/accounts/%d", id), map[string]any{"status": "disabled"}); code != 200 || data(out)["admin_locked"] != false {
		t.Fatalf("disable own: %d %v", code, out)
	}
}

// subordinates=true narrows the list to the accounts of the users below the
// caller (its account:read team, core.ResolveScope), never its own; without
// a team scope it lists none. A team reads, but does not write, what it
// reaches beyond its own (D4).
func TestAccountSubordinates(t *testing.T) {
	az := fakeAuthz{keys: map[int64][]string{}, teams: map[int64][]int64{}}
	e := setupWith(t, az)
	admin := e.uid
	agent, sub, other := e.addUser("agent@x.com"), e.addUser("sub@x.com"), e.addUser("other@x.com")
	az.keys[admin] = adminKeys
	az.keys[agent] = append(slices.Clone(supplierKeys), "account:read:team")
	az.keys[sub], az.keys[other] = supplierKeys, supplierKeys
	az.teams[agent] = []int64{sub}
	ids := map[int64]string{}
	for _, uid := range []int64{agent, sub, other} {
		ids[uid] = strconv.FormatInt(int64(e.createAs(uid, "acct", nil)["id"].(float64)), 10)
	}
	for _, tc := range []struct {
		uid   int64
		query string
		want  string
	}{
		{agent, "", ids[sub] + "," + ids[agent]},
		{agent, "?subordinates=true", ids[sub]},
		{agent, "?mine=true", ids[agent]},
		{other, "?subordinates=true", ""},
		{admin, "?subordinates=true", ""},
	} {
		if _, out := e.doAs(tc.uid, "GET", "/accounts"+tc.query, nil); listIDs(out) != tc.want {
			t.Errorf("%d %s: %v, want %s", tc.uid, tc.query, listIDs(out), tc.want)
		}
	}
	if code, out := e.doAs(agent, "PATCH", "/accounts/"+ids[sub], map[string]any{"name": "x"}); code != 404 {
		t.Fatalf("agent writes a subordinate's account: %d %v", code, out)
	}
}

// A scope a handler reads beyond its route's (here proxy:manage, for a
// proxy_url) whose team cannot be read fails the request with 503: never
// an empty scope, which would have refused it as if the caller held none.
func TestAccountScopeUnavailable(t *testing.T) {
	az := fakeAuthz{keys: map[int64][]string{}, teamErr: core.ErrUnavailable}
	e := setupWith(t, az)
	agent := e.addUser("agent@x.com")
	az.keys[agent] = append(slices.DeleteFunc(slices.Clone(supplierKeys), func(k string) bool { return k == "proxy:manage:own" }), "proxy:manage:team")
	body := map[string]any{"name": "a", "plugin_key": "anthropic", "type": "apikey",
		"credentials": map[string]any{"api_key": "sk-good-key-123"}, "proxy_url": "socks5h://proxy.example:1080"}
	if code, out := e.doAs(agent, "POST", "/accounts", body); code != 503 {
		t.Fatalf("create with a proxy_url: %d %v", code, out)
	}
}

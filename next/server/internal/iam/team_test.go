package iam

import (
	"context"
	"strconv"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// teamEnv is keyEnv with two supplier agents: agent owns sub, other owns
// otherSub; outsider is a supplier with no owner.
type teamEnv struct {
	*keyEnv
	root, agent, sub, other, otherSub, outsider int64
	rootTok, agentTok                           string
}

func newTeamEnv(t *testing.T) *teamEnv {
	t.Helper()
	k := newKeyEnv(t)
	e := &teamEnv{keyEnv: k}
	mk := func(email, role string, owner int64) int64 {
		t.Helper()
		u, err := k.s.CreateUser(context.Background(), 0, CreateUserInput{Email: email, Password: adminPassword, RoleKeys: []string{role}, OwnerID: &owner})
		if err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	e.root = rootID(t, k.s)
	e.agent = mk("agent@example.com", authz.RoleSupplierAgent, 0)
	e.sub = mk("sub@example.com", authz.RoleSupplier, e.agent)
	e.other = mk("other@example.com", authz.RoleSupplierAgent, 0)
	e.otherSub = mk("other-sub@example.com", authz.RoleSupplier, e.other)
	e.outsider = mk("outsider@example.com", authz.RoleSupplier, 0)
	e.rootTok = mustLogin(t, k.s, adminEmail, adminPassword).AccessToken
	e.agentTok = mustLogin(t, k.s, "agent@example.com", adminPassword).AccessToken
	k.owners.calls = nil
	return e
}

func (e *teamEnv) path(id int64) string { return "/users/" + strconv.FormatInt(id, 10) }

// ids lists the users of GET path as token, in order.
func (e *teamEnv) ids(t *testing.T, token, path string) []int64 {
	t.Helper()
	code, out := e.call(token, "GET", path, nil)
	if code != 200 {
		t.Fatalf("GET %s: %d %v", path, code, out)
	}
	ids := []int64{}
	for _, u := range out["data"].([]any) {
		ids = append(ids, int64(u.(map[string]any)["id"].(float64)))
	}
	return ids
}

func equalIDs(t *testing.T, what string, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: %v, want %v", what, got, want)
		}
	}
}

// A holder of user:read:team sees the users below it and no other, itself
// included, through every filter and per-user route (CONTRACTS §103.3).
func TestUsersTeamScope(t *testing.T) {
	t.Parallel()
	e := newTeamEnv(t)
	agent, other := strconv.FormatInt(e.agent, 10), strconv.FormatInt(e.other, 10)

	equalIDs(t, "team", e.ids(t, e.agentTok, "/users"), e.sub)
	equalIDs(t, "direct", e.ids(t, e.agentTok, "/users?direct=true"), e.sub)
	equalIDs(t, "owned by the caller", e.ids(t, e.agentTok, "/users?owner_id="+agent), e.sub)
	equalIDs(t, "owned by another agent", e.ids(t, e.agentTok, "/users?owner_id="+other))
	equalIDs(t, "without an owner", e.ids(t, e.agentTok, "/users?owner_id=none"))
	equalIDs(t, "found by email out of the team", e.ids(t, e.agentTok, "/users?q=outsider"))

	equalIDs(t, "all: owned by another agent", e.ids(t, e.rootTok, "/users?owner_id="+other), e.otherSub)
	equalIDs(t, "all: without an owner", e.ids(t, e.rootTok, "/users?owner_id=none&q=example.com"), e.root, e.userID, e.agent, e.other, e.outsider)
	equalIDs(t, "all: direct is the caller's", e.ids(t, e.rootTok, "/users?direct=true"))
	for _, q := range []string{"owner_id=0", "owner_id=-3", "owner_id=x", "direct=maybe"} {
		if code, out := e.call(e.rootTok, "GET", "/users?"+q, nil); code != 400 || errCode(out) != "invalid_argument" {
			t.Fatalf("%s: %d %v", q, code, out)
		}
	}

	// A deleted user leaves the list.
	gone, err := e.s.CreateUser(context.Background(), 0, CreateUserInput{Email: "gone@example.com", Password: adminPassword, RoleKeys: []string{authz.RoleSupplier}, OwnerID: &e.agent})
	if err != nil {
		t.Fatal(err)
	}
	e.owners.calls = nil
	equalIDs(t, "team with a new user", e.ids(t, e.agentTok, "/users"), e.sub, gone.ID)
	if err := e.s.DeleteUser(context.Background(), e.root, gone.ID); err != nil {
		t.Fatal(err)
	}
	equalIDs(t, "team after a delete", e.ids(t, e.agentTok, "/users"), e.sub)

	code, out := e.call(e.agentTok, "GET", e.path(e.sub), nil)
	data, _ := out["data"].(map[string]any)
	if code != 200 || data["owner_id"] != float64(e.agent) || data["owner_email"] != "agent@example.com" {
		t.Fatalf("GET own sub: %d %v", code, out)
	}
	name := "Renamed"
	for _, id := range []int64{e.outsider, e.otherSub, e.agent, e.root, 999999} {
		if code, out := e.call(e.agentTok, "GET", e.path(id), nil); code != 404 {
			t.Fatalf("GET %d: %d %v", id, code, out)
		}
		if code, out := e.call(e.agentTok, "PATCH", e.path(id), map[string]any{"display_name": name}); code != 404 {
			t.Fatalf("PATCH %d: %d %v", id, code, out)
		}
	}
	if u, err := e.s.GetUser(context.Background(), e.outsider); err != nil || u.DisplayName == name {
		t.Fatalf("the outsider was renamed: %v %v", u, err)
	}

	// Within the team: update, disable and enable, but no password reset,
	// delete (D5) or members list, which take unscoped keys.
	for _, body := range []map[string]any{{"display_name": name}, {"status": StatusDisabled}, {"status": StatusActive}} {
		if code, out := e.call(e.agentTok, "PATCH", e.path(e.sub), body); code != 200 {
			t.Fatalf("PATCH sub %v: %d %v", body, code, out)
		}
	}
	if code, out := e.call(e.agentTok, "PATCH", e.path(e.sub), map[string]any{"password": "another-password-1"}); code != 403 || errCode(out) != "permission_denied" {
		t.Fatalf("password reset: %d %v", code, out)
	}
	if code, out := e.call(e.agentTok, "DELETE", e.path(e.sub), nil); code != 403 {
		t.Fatalf("DELETE sub: %d %v", code, out)
	}
	if code, out := e.call(e.agentTok, "GET", "/segments/1/members", nil); code != 403 {
		t.Fatalf("segment members: %d %v", code, out)
	}
	if len(e.owners.calls) != 0 {
		t.Fatalf("CheckOwner called without owner_id: %+v", e.owners.calls)
	}
}

// The owner of a new or updated user is the one CheckOwner gives, judged
// in the same transaction; a team-only creator owns what it creates
// (CONTRACTS §103.3).
func TestUserOwner(t *testing.T) {
	t.Parallel()
	e := newTeamEnv(t)
	ownerOf := func(id int64) any {
		t.Helper()
		_, out := e.call(e.rootTok, "GET", e.path(id), nil)
		return out["data"].(map[string]any)["owner_id"]
	}
	lastCall := func() ownerCall {
		t.Helper()
		if len(e.owners.calls) == 0 {
			t.Fatal("CheckOwner not called")
		}
		return e.owners.calls[len(e.owners.calls)-1]
	}

	// A team-only creator: itself by default.
	code, out := e.call(e.agentTok, "POST", "/users", map[string]any{"email": "made@example.com", "password": adminPassword, "role_keys": []string{authz.RoleSupplier}})
	if code != 201 || out["data"].(map[string]any)["owner_id"] != float64(e.agent) {
		t.Fatalf("agent creates: %d %v", code, out)
	}
	made := int64(out["data"].(map[string]any)["id"].(float64))
	if c := lastCall(); !c.create || c.actor != e.agent || c.user != made || c.owner != nil {
		t.Fatalf("CheckOwner on create: %+v", c)
	}
	equalIDs(t, "team with the new user", e.ids(t, e.agentTok, "/users"), e.sub, made)

	// The full scope: none by default, any owner named.
	if code, out := e.call(e.rootTok, "POST", "/users", map[string]any{"email": "plain@example.com", "password": adminPassword}); code != 201 || out["data"].(map[string]any)["owner_id"] != nil {
		t.Fatalf("root creates: %d %v", code, out)
	}
	if c := lastCall(); c.owner != nil {
		t.Fatalf("CheckOwner on a default create: %+v", c)
	}
	code, out = e.call(e.rootTok, "POST", "/users", map[string]any{"email": "placed@example.com", "password": adminPassword, "owner_id": e.other})
	if code != 201 || out["data"].(map[string]any)["owner_email"] != "other@example.com" {
		t.Fatalf("root creates under other: %d %v", code, out)
	}

	// A refusal of CheckOwner rolls the create back.
	e.owners.err = core.ErrPermissionDenied
	if code, out := e.call(e.agentTok, "POST", "/users", map[string]any{"email": "escape@example.com", "password": adminPassword, "role_keys": []string{authz.RoleSupplier}, "owner_id": e.other}); code != 403 {
		t.Fatalf("refused create: %d %v", code, out)
	}
	e.owners.err = nil
	var n int
	if err := e.s.db.SQL.QueryRow(`SELECT count(*) FROM users WHERE email = 'escape@example.com'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused create left %d users (%v)", n, err)
	}
	// Naming anyone but itself is refused before the owner is looked up: a
	// team-only creator cannot tell a user id that exists from one that
	// does not.
	for _, owner := range []int64{e.other, 999999} {
		code, out := e.call(e.agentTok, "POST", "/users", map[string]any{"email": "probe@example.com", "password": adminPassword, "owner_id": owner})
		if code != 403 || errCode(out) != "permission_denied" {
			t.Fatalf("agent names %d: %d %v", owner, code, out)
		}
	}
	for owner, code := range map[int64]string{-1: "invalid", 999999: "not_found"} {
		_, out := e.call(e.rootTok, "POST", "/users", map[string]any{"email": "bad-owner@example.com", "password": adminPassword, "owner_id": owner})
		fields, _ := out["error"].(map[string]any)["details"].(map[string]any)["fields"].([]any)
		if errCode(out) != "invalid_argument" || len(fields) != 1 || fields[0].(map[string]any)["field"] != "owner_id" || fields[0].(map[string]any)["code"] != code {
			t.Fatalf("owner %d: %v", owner, out)
		}
	}

	// PATCH: moved, cleared, or kept when not given.
	e.owners.calls = nil
	if code, out := e.call(e.rootTok, "PATCH", e.path(e.outsider), map[string]any{"owner_id": e.agent}); code != 200 || ownerOf(e.outsider) != float64(e.agent) {
		t.Fatalf("move: %d %v", code, out)
	}
	if c := lastCall(); c.create || c.actor != e.root || c.user != e.outsider || *c.owner != e.agent {
		t.Fatalf("CheckOwner on update: %+v", c)
	}
	equalIDs(t, "team with the moved user", e.ids(t, e.agentTok, "/users"), e.sub, e.outsider, made)
	if code, out := e.call(e.rootTok, "PATCH", e.path(e.outsider), map[string]any{"display_name": "Kept", "owner_id": nil}); code != 200 || ownerOf(e.outsider) != float64(e.agent) || len(e.owners.calls) != 1 {
		t.Fatalf("no owner given: %d %v %+v", code, out, e.owners.calls)
	}
	if code, out := e.call(e.rootTok, "PATCH", e.path(e.outsider), map[string]any{"owner_id": 0}); code != 200 || ownerOf(e.outsider) != nil {
		t.Fatalf("clear: %d %v", code, out)
	}
	e.owners.err = core.ErrPermissionDenied
	if code, out := e.call(e.agentTok, "PATCH", e.path(e.sub), map[string]any{"display_name": "Escaped", "owner_id": e.other}); code != 403 || ownerOf(e.sub) != float64(e.agent) {
		t.Fatalf("refused move: %d %v", code, out)
	}
	e.owners.err = nil

	// The email of a deleted owner stays.
	if err := e.s.DeleteUser(context.Background(), e.root, e.other); err != nil {
		t.Fatal(err)
	}
	if _, out := e.call(e.rootTok, "GET", e.path(e.otherSub), nil); out["data"].(map[string]any)["owner_email"] != "other@example.com" {
		t.Fatalf("owner deleted: %v", out)
	}
}

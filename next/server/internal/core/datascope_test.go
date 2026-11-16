package core

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/permvectors"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
)

func scopesOf(t *testing.T, keys ...string) []perm.Scope {
	t.Helper()
	var out []perm.Scope
	for _, k := range keys {
		p, err := perm.Parse(k)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		out = append(out, p.Scope)
	}
	return out
}

func TestCompile(t *testing.T) {
	t.Parallel()
	got := Compile(7, scopesOf(t,
		"account:read:group/12", "account:read:account/5", "account:read:own", "account:read:group/*/account/9",
		"account:read:group/3/account/9", "account:read:group/12", "account:read:account/2", "account:read:group/*"))
	want := DataScope{Owners: []int64{7}, Groups: []int64{0, 12}, IDs: []int64{2, 5}, PairG: []int64{0, 3}, PairID: []int64{9, 9}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Compile = %+v, want %+v", got, want)
	}
	if !got.Restricted() || got.Empty() || !got.Grouped() {
		t.Fatal("flags")
	}
	if all := Compile(7, scopesOf(t, "account:read:own", "account:read")); !reflect.DeepEqual(all, AllScope) || all.Restricted() {
		t.Fatalf("all = %+v", all)
	}
	if none := Compile(7, nil); !none.Empty() || !none.Restricted() {
		t.Fatalf("none = %+v", none)
	}
	if own := Compile(7, scopesOf(t, "account:read:own")); own.Grouped() || !own.Owned() || !reflect.DeepEqual(own.Owners, []int64{7}) {
		t.Fatalf("own = %+v", own)
	}
	// A team compiles to the holder, as an owner and as the top of a team
	// (the users below it read by the statement); a union keeps it.
	if team := Compile(7, scopesOf(t, "account:read:team", "account:read:own")); !reflect.DeepEqual(team, DataScope{Owners: []int64{7}, Teams: []int64{7}}) {
		t.Fatalf("team = %+v", team)
	}
	if u := Compile(7, scopesOf(t, "account:read:own")).Union(Compile(7, scopesOf(t, "account:read:team"))); !reflect.DeepEqual(u, DataScope{Owners: []int64{7}, Teams: []int64{7}}) {
		t.Fatalf("union = %+v", u)
	}
	if Compile(7, scopesOf(t, "account:read:group/1")).Owned() {
		t.Fatal("a group range is not owned")
	}
}

func TestAllowsRow(t *testing.T) {
	t.Parallel()
	me, other := int64(7), int64(8)
	cases := []struct {
		keys   []string
		id     int64
		owner  *int64
		groups []int64
		want   bool
	}{
		{[]string{"account:read"}, 1, nil, nil, true},
		{[]string{"account:read:own"}, 1, &me, nil, true},
		{[]string{"account:read:team"}, 1, &me, nil, true},
		{[]string{"account:read:team"}, 1, &other, nil, false},
		{[]string{"account:read:own"}, 1, &other, nil, false},
		{[]string{"account:read:own"}, 1, nil, nil, false},
		{[]string{"account:read:account/1"}, 1, nil, nil, true},
		{[]string{"account:read:account/1"}, 2, nil, []int64{1}, false},
		{[]string{"account:read:group/12"}, 1, nil, []int64{3, 12}, true},
		{[]string{"account:read:group/12"}, 1, nil, []int64{3}, false},
		{[]string{"account:read:group/*"}, 1, nil, []int64{3}, true},
		{[]string{"account:read:group/*"}, 1, nil, nil, false},
		{[]string{"account:read:group/12/account/1"}, 1, nil, []int64{12}, true},
		{[]string{"account:read:group/12/account/1"}, 1, nil, []int64{13}, false},
		{[]string{"account:read:group/12/account/1"}, 2, nil, []int64{12}, false},
		{[]string{"account:read:group/*/account/1"}, 1, nil, []int64{13}, true},
		{[]string{"account:read:group/*/account/1"}, 1, nil, nil, false},
		{nil, 1, &me, []int64{1}, false},
	}
	for _, c := range cases {
		if got := Compile(me, scopesOf(t, c.keys...)).AllowsRow(c.id, c.owner, c.groups); got != c.want {
			t.Errorf("%v row %d of %v in %v: %v, want %v", c.keys, c.id, c.owner, c.groups, got, c.want)
		}
	}
}

// The shared vectors (sdk/permvectors "allows") pin the row check the
// console's scopeAllowsRow repeats: the keys held, compiled for the action,
// against one row.
func TestAllowsRowVectors(t *testing.T) {
	t.Parallel()
	var v struct {
		Allows []struct {
			Have   []string `json:"have"`
			Action string   `json:"action"`
			Me     int64    `json:"me"`
			Row    struct {
				ID        int64   `json:"id"`
				CreatedBy *int64  `json:"created_by"`
				GroupIDs  []int64 `json:"group_ids"`
			} `json:"row"`
			OK bool `json:"ok"`
		}
	}
	if err := json.Unmarshal(permvectors.JSON, &v); err != nil || len(v.Allows) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, c := range v.Allows {
		s := PermissionSetOf(c.Have...).DataScope(c.Me, perm.Key(c.Action))
		if got := s.AllowsRow(c.Row.ID, c.Row.CreatedBy, c.Row.GroupIDs); got != c.OK {
			t.Errorf("%v %s by %d on %+v: %v, want %v", c.Have, c.Action, c.Me, c.Row, got, c.OK)
		}
	}
}

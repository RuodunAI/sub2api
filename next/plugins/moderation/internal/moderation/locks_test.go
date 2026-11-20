package moderation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// lockUser puts uid on list through the API.
func lockUser(t *testing.T, h *pluginsdktest.Harness, uid int64, list string) Watch {
	t.Helper()
	return decodeData[Watch](t, h.Do("PUT", fmt.Sprintf("/watchlist/%d/lock", uid), nil, map[string]any{"list": list}))
}

// PUT /watchlist/:user_id/lock makes the user's entry, or a new one, a
// locked entry of the list (no expiry, source manual), once; DELETE removes
// a locked entry. Every change is one history row by the caller.
func TestLockAPI(t *testing.T) {
	p, h, fh := startDB(t, map[string]any{"mode": ModeObserve})
	watchVersion := func() (v int64) {
		t.Helper()
		db, err := p.db(context.Background())
		if err == nil {
			err = db.Raw(`SELECT watch_version FROM plg_moderation__config_state WHERE id = 1`).Row().Scan(&v)
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	decodeData[Watch](t, h.Do("POST", "/watchlist", nil, map[string]any{"user_id": 5, "level": LevelObserve, "days": 3}))
	if w := lockUser(t, h, 5, LockBlocklist); !w.Locked || w.Lock != LockBlocklist || w.Level != LevelEnforce || w.ExpiresAt != nil || w.Source != "manual" {
		t.Fatalf("locked entry = %+v", w)
	}
	version := watchVersion()
	if w := lockUser(t, h, 5, LockBlocklist); w.Lock != LockBlocklist || watchVersion() != version { // already one: nothing written
		t.Fatalf("unchanged lock = %+v, watch_version %d -> %d", w, version, watchVersion())
	}
	if w := lockUser(t, h, 5, LockAllowlist); w.Lock != LockAllowlist || w.Level != LevelObserve || w.ExpiresAt != nil {
		t.Fatalf("moved to the allowlist = %+v", w)
	}
	lockUser(t, h, 7, LockBlocklist)
	evs := historyOf(t, h, 5)
	if got := actions(evs); got != "add:>observe,lock:observe>enforce,lock:enforce>observe" {
		t.Fatalf("history of 5 = %s", got)
	}
	if last := evs[2]; last.Reason != lockLists[LockAllowlist].added || last.OperatorID == nil || *last.OperatorID != 1 {
		t.Fatalf("lock row = %+v", last)
	}
	if got := actions(historyOf(t, h, 7)); got != "lock:>enforce" {
		t.Fatalf("history of 7 = %s", got)
	}
	var counts struct {
		Counts WatchCounts `json:"counts"`
	}
	if err := json.Unmarshal(h.Do("GET", "/watchlist", nil, nil).GetBody(), &counts); err != nil || counts.Counts.Locked != 2 || counts.Counts.Blocking != 1 {
		t.Fatalf("counts = %+v (%v)", counts.Counts, err)
	}

	decodeData[Watch](t, h.Do("POST", "/watchlist", nil, map[string]any{"user_id": 6}))
	fh.Authz = func(uid int64, _ string) bool { return uid == 30 }
	for _, tc := range []struct {
		method, path string
		body         any
		status       int
		code         string
	}{
		{"PUT", "/watchlist/8/lock", map[string]any{"list": "watch"}, 400, `"enum"`},
		{"PUT", "/watchlist/8/lock", nil, 400, "invalid_argument"},
		{"PUT", "/watchlist/30/lock", map[string]any{"list": LockBlocklist}, 400, `"staff"`},
		{"PUT", "/watchlist/1000/lock", map[string]any{"list": LockBlocklist}, 404, "user_not_found"},
		{"DELETE", "/watchlist/6/lock", nil, 404, "not_locked"},
		{"DELETE", "/watchlist/8/lock", nil, 404, "not_locked"},
	} {
		if resp := h.Do(tc.method, tc.path, nil, tc.body); resp.GetStatus() != int32(tc.status) || !strings.Contains(string(resp.GetBody()), tc.code) {
			t.Errorf("%s %s %v = %d %s", tc.method, tc.path, tc.body, resp.GetStatus(), resp.GetBody())
		}
	}

	if got := decodeData[map[string]any](t, h.Do("DELETE", "/watchlist/7/lock", nil, nil)); got["removed"] != true {
		t.Fatalf("unlock = %v", got)
	}
	evs = historyOf(t, h, 7)
	if actions(evs) != "lock:>enforce,unlock:enforce>" || evs[1].Reason != lockLists[LockBlocklist].removed || evs[1].OperatorID == nil {
		t.Fatalf("history of 7 = %+v", evs)
	}
	if d := decodeData[UserDetail](t, h.Do("GET", "/users/7", nil, nil)); d.Watch != nil || d.Blocklisted {
		t.Fatalf("unlocked user = %+v", d.UserState)
	}
}

// GET /watchlist marks the entries of moderators (made before the user
// became one): the console offers no lock for them.
func TestWatchListStaff(t *testing.T) {
	p, h, fh := startDB(t, map[string]any{"mode": ModeObserve})
	lockUser(t, h, 5, LockBlocklist)
	fh.Authz = func(uid int64, _ string) bool { return uid == 6 }
	ctx := context.Background()
	db, err := p.db(ctx)
	if err == nil {
		err = db.Exec(`INSERT INTO plg_moderation__watchlist (user_id, level, source, since_at, violation_epoch, locked, updated_at)
			VALUES (6, 'observe', 'manual', now(), 1, true, now())`).Error
	}
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []WatchItem `json:"data"`
	}
	if err := json.Unmarshal(h.Do("GET", "/watchlist", nil, nil).GetBody(), &out); err != nil {
		t.Fatal(err)
	}
	staff := map[int64]bool{}
	for _, it := range out.Data {
		staff[it.UserID] = it.Staff
	}
	if len(staff) != 2 || !staff[6] || staff[5] {
		t.Fatalf("staff = %v", staff)
	}
}

package moderation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// The one-time 0.8.0 conversion: under mode = enforce the live automatic
// observe entries become enforce entries with a history row each; manual
// and AI entries, expired entries and enforce entries stay. It runs once per
// installation, on one node.
func TestParityConversionOnce(t *testing.T) {
	ctx := context.Background()
	prepare := func(t *testing.T, p *Plugin, rows string) {
		t.Helper()
		// The start already ran the conversion on the empty list.
		waitFor(t, "conversion at start", p.parityDone.Load)
		db, _ := p.db(ctx)
		if err := db.Exec(`INSERT INTO plg_moderation__watchlist (user_id, level, source, expires_at, violation_count) VALUES ` + rows).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`UPDATE plg_moderation__config_state SET parity_converted = false, parity_converted_at = NULL WHERE id = 1`).Error; err != nil {
			t.Fatal(err)
		}
	}
	levels := func(t *testing.T, p *Plugin) map[int64]string {
		t.Helper()
		db, _ := p.db(ctx)
		out := map[int64]string{}
		rows, err := db.Raw(`SELECT user_id, level || '/' || source || '/' || violation_count FROM plg_moderation__watchlist`).Rows()
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var uid int64
			var v string
			if err := rows.Scan(&uid, &v); err != nil {
				t.Fatal(err)
			}
			out[uid] = v
		}
		return out
	}
	converted := func(t *testing.T, p *Plugin) bool {
		t.Helper()
		db, _ := p.db(ctx)
		var done bool
		if err := db.Raw(`SELECT parity_converted FROM plg_moderation__config_state WHERE id = 1`).Row().Scan(&done); err != nil {
			t.Fatal(err)
		}
		return done
	}
	const rows = `(21, 'observe', 'auto', now() + interval '5 days', 4),
		(22, 'observe', 'auto', NULL, 0),
		(23, 'observe', 'manual', now() + interval '5 days', 1),
		(24, 'observe', 'analysis', now() + interval '5 days', 2),
		(25, 'observe', 'auto', now() - interval '1 minute', 3),
		(26, 'enforce', 'auto', now() + interval '5 days', 6)`

	t.Run("mode enforce, two nodes", func(t *testing.T) {
		p, h, fh := startDB(t, map[string]any{"mode": ModeEnforce}, noRefreshTicks)
		fh2 := pluginsdktest.NewFakeHost()
		fh2.SetDSN(fh.DSNValue, fh.SchemaValue)
		p2, _ := startNode(t, fh2, map[string]any{"mode": ModeEnforce}, noRefreshTicks)
		waitFor(t, "conversion at start of the second node", p2.parityDone.Load)
		prepare(t, p, rows)
		db, _ := p.db(ctx)
		var expires time.Time
		if err := db.Raw(`SELECT expires_at FROM plg_moderation__watchlist WHERE user_id = 21`).Row().Scan(&expires); err != nil {
			t.Fatal(err)
		}
		// Both nodes convert at the same time: the row lock lets one do it.
		p.parityDone.Store(false)
		p2.parityDone.Store(false)
		var wg sync.WaitGroup
		for _, node := range []*Plugin{p, p2, p, p2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				node.convertParityOnce(ctx)
			}()
		}
		wg.Wait()
		want := map[int64]string{21: "enforce/auto/4", 22: "enforce/auto/0", 23: "observe/manual/1", 24: "observe/analysis/2",
			25: "observe/auto/3", 26: "enforce/auto/6"}
		if got := levels(t, p); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("watch list = %v, want %v", got, want)
		}
		if !converted(t, p) || !p.parityDone.Load() || !p2.parityDone.Load() {
			t.Fatal("conversion not marked done")
		}
		// One history row for each converted entry, without an operator; the
		// expiry is untouched.
		history := func() string {
			t.Helper()
			var out []string
			rows, err := db.Raw(`SELECT user_id, action, from_level, to_level, operator_id IS NULL, reason FROM plg_moderation__watch_events ORDER BY user_id, id`).Rows()
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var uid int64
				var action, from, to, reason string
				var auto bool
				if err := rows.Scan(&uid, &action, &from, &to, &auto, &reason); err != nil {
					t.Fatal(err)
				}
				if reason != parityReason {
					t.Fatalf("reason = %q", reason)
				}
				out = append(out, fmt.Sprintf("%d %s %s>%s %v", uid, action, from, to, auto))
			}
			return strings.Join(out, "; ")
		}
		const wantHistory = "21 set_level observe>enforce true; 22 set_level observe>enforce true"
		if got := history(); got != wantHistory {
			t.Fatalf("history = %s", got)
		}
		var after time.Time
		if err := db.Raw(`SELECT expires_at FROM plg_moderation__watchlist WHERE user_id = 21`).Row().Scan(&after); err != nil || !after.Equal(expires) {
			t.Fatalf("expiry %v -> %v (%v)", expires, after, err)
		}
		// The hooks of both nodes see the new levels.
		for _, node := range []*Plugin{p, p2} {
			waitFor(t, "converted levels in the hook", func() bool {
				_ = node.pollVersions(ctx)
				return node.watchLevel(21) == LevelEnforce && node.watchLevel(22) == LevelEnforce && node.watchLevel(23) == LevelObserve
			})
		}
		if ev := decodeData[[]WatchEvent](t, h.Do("GET", "/watchlist/21/events", nil, nil)); len(ev) != 1 || ev[0].OperatorID != nil {
			t.Fatalf("history of user 21 = %+v", ev)
		}
		// Once the day after the conversion is over (TestParityConversionWindow)
		// it never runs again: a later automatic observe entry stays, also
		// after a restart (parityDone cleared).
		if err := db.Exec(`UPDATE plg_moderation__config_state SET parity_converted_at = now() - interval '25 hours' WHERE id = 1`).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO plg_moderation__watchlist (user_id, level, source, expires_at) VALUES (27, 'observe', 'auto', now() + interval '5 days')`).Error; err != nil {
			t.Fatal(err)
		}
		p.parityDone.Store(false)
		p.convertParityOnce(ctx)
		want[27] = "observe/auto/0"
		if got := levels(t, p); fmt.Sprint(got) != fmt.Sprint(want) || history() != wantHistory || !p.parityDone.Load() {
			t.Fatalf("second run changed the list: %v; history %s", got, history())
		}
	})

	t.Run("mode observe", func(t *testing.T) {
		p, h, _ := startDB(t, map[string]any{"mode": ModeObserve}, noRefreshTicks)
		prepare(t, p, rows)
		p.parityDone.Store(false)
		p.convertParityOnce(ctx)
		want := map[int64]string{21: "observe/auto/4", 22: "observe/auto/0", 23: "observe/manual/1", 24: "observe/analysis/2",
			25: "observe/auto/3", 26: "enforce/auto/6"}
		if got := levels(t, p); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("watch list = %v, want %v", got, want)
		}
		if !converted(t, p) || !p.parityDone.Load() {
			t.Fatal("conversion not marked done")
		}
		// The mode is only looked at once: switching to enforce later
		// converts nothing.
		if errs := h.Configure(map[string]any{"mode": ModeEnforce}, nil); len(errs) > 0 {
			t.Fatal(errs)
		}
		p.parityDone.Store(false)
		p.convertParityOnce(ctx)
		if got := levels(t, p); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("watch list after the mode change = %v, want %v", got, want)
		}
		db, _ := p.db(ctx)
		var n int
		if err := db.Raw(`SELECT count(*) FROM plg_moderation__watch_events`).Row().Scan(&n); err != nil || n != 0 {
			t.Fatalf("history rows = %d %v", n, err)
		}
	})
}

// For a day after the conversion, automatic observe entries added since (by
// nodes still on 0.7.x) are converted too; after it never again.
func TestParityConversionWindow(t *testing.T) {
	ctx := context.Background()
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	p, h, _ := startDB(t, map[string]any{"mode": ModeEnforce}, func(n *node) { n.now = clock }, noRefreshTicks)
	waitFor(t, "conversion at start", p.parityDone.Load)
	db, _ := p.db(ctx)
	var at *time.Time
	if err := db.Raw(`SELECT parity_converted_at FROM plg_moderation__config_state WHERE id = 1 AND parity_converted`).Row().Scan(&at); err != nil || at == nil ||
		time.Since(*at) > time.Minute {
		t.Fatalf("conversion time = %v (%v)", at, err)
	}
	levels := func() string {
		t.Helper()
		var list []string
		if err := db.Raw(`SELECT user_id || ':' || level FROM plg_moderation__watchlist ORDER BY user_id`).Scan(&list).Error; err != nil {
			t.Fatal(err)
		}
		return strings.Join(list, " ")
	}
	add := func(rows string) {
		t.Helper()
		if err := db.Exec(`INSERT INTO plg_moderation__watchlist (user_id, level, source, since_at, expires_at) VALUES ` + rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	// What a 0.7.x node adds after the conversion (31), an administrator's
	// observe entry (32), an automatic entry from before the conversion
	// (33, left alone by a first run under mode = observe) and an expired
	// one (34).
	add(`(31, 'observe', 'auto', now(), now() + interval '5 days'),
		(32, 'observe', 'manual', now(), now() + interval '5 days'),
		(33, 'observe', 'auto', now() - interval '1 hour', now() + interval '5 days'),
		(34, 'observe', 'auto', now(), now() - interval '1 second')`)
	// Under mode = observe nothing is converted.
	if errs := h.Configure(map[string]any{"mode": ModeObserve}, nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	p.convertParityOnce(ctx)
	if got := levels(); got != "31:observe 32:observe 33:observe 34:observe" {
		t.Fatalf("mode observe converted: %s", got)
	}
	if errs := h.Configure(map[string]any{"mode": ModeEnforce}, nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	// The loop converts it without a restart (parityDone stays set).
	if !p.parityDone.Load() {
		t.Fatal("parityDone cleared")
	}
	p.convertParityOnce(ctx)
	p.convertParityOnce(ctx)
	if got := levels(); got != "31:enforce 32:observe 33:observe 34:observe" {
		t.Fatalf("in the window: %s", got)
	}
	ev := decodeData[[]WatchEvent](t, h.Do("GET", "/watchlist/31/events", nil, nil))
	if len(ev) != 1 || ev[0].Action != "set_level" || ev[0].ToLevel != LevelEnforce || ev[0].Reason != parityReason || ev[0].OperatorID != nil {
		t.Fatalf("history of user 31 = %+v", ev)
	}
	waitFor(t, "the new level in the hook", func() bool { return p.watchLevel(31) == LevelEnforce })
	// Still inside the day, 23 hours later.
	offset.Store(int64(23 * time.Hour))
	add(`(35, 'observe', 'auto', now() + interval '23 hours', now() + interval '5 days')`)
	p.convertParityOnce(ctx)
	if got := levels(); !strings.Contains(got, "35:enforce") {
		t.Fatalf("after 23 hours: %s", got)
	}
	// After the day: nothing more, on this run or after a restart, and the
	// loop no longer asks the database.
	offset.Store(int64(25 * time.Hour))
	add(`(36, 'observe', 'auto', now() + interval '25 hours', now() + interval '5 days')`)
	p.convertParityOnce(ctx)
	p.parityDone.Store(false)
	p.convertParityOnce(ctx)
	if got := levels(); !strings.Contains(got, "36:observe") || !p.parityDone.Load() {
		t.Fatalf("after 25 hours: %s", got)
	}
	if err := db.Exec(`UPDATE plg_moderation__config_state SET parity_converted_at = now() + interval '30 hours' WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	p.convertParityOnce(ctx) // would convert 36 if it read the new time
	if got := levels(); !strings.Contains(got, "36:observe") {
		t.Fatalf("the finished conversion ran again: %s", got)
	}
}

// The conversion takes its locks in the order of every other watch change
// (the user's lock and entry, then config_state): a change in progress on an
// entry it wants is waited for, not deadlocked with.
func TestParityConversionLockOrder(t *testing.T) {
	ctx := context.Background()
	p, _, _ := startDB(t, map[string]any{"mode": ModeEnforce}, noRefreshTicks)
	waitFor(t, "conversion at start", p.parityDone.Load)
	db, _ := p.db(ctx)
	if err := db.Exec(`INSERT INTO plg_moderation__watchlist (user_id, level, source, expires_at) VALUES (41, 'observe', 'auto', now() + interval '5 days')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE plg_moderation__config_state SET parity_converted = false, parity_converted_at = NULL WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	// A watch change of user 41 in progress, as writeWatch does it: the
	// user's lock, the entry, and config_state last.
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := lockBlockSubject(tx, 41); err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`UPDATE plg_moderation__watchlist SET note = 'in progress' WHERE user_id = 41`).Error; err != nil {
		t.Fatal(err)
	}
	p.parityDone.Store(false)
	converted := make(chan struct{})
	go func() {
		defer close(converted)
		p.convertParityOnce(ctx)
	}()
	// The conversion waits for the user's lock (the database shows its
	// statement waiting), and has not finished.
	waitFor(t, "the conversion to wait for the user's lock", func() bool {
		var waiting int
		if err := db.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%block_subjects%' AND pid <> pg_backend_pid()`).Row().Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		return waiting > 0
	})
	select {
	case <-converted:
		t.Fatal("the conversion did not wait for the change in progress")
	default:
	}
	// The change reaches config_state at once: the conversion does not hold
	// its row while it waits for the entry (NOWAIT fails on a held row
	// instead of waiting for it).
	if err := tx.Exec(`SELECT 1 FROM plg_moderation__config_state WHERE id = 1 FOR UPDATE NOWAIT`).Error; err != nil {
		t.Fatalf("config_state is held while the conversion waits: %v", err)
	}
	if err := bumpWatch(tx); err != nil {
		t.Fatalf("the watch change could not finish: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	within(t, "the conversion", converted)
	var level, note string
	var done bool
	if err := db.Raw(`SELECT w.level, w.note, s.parity_converted FROM plg_moderation__watchlist w, plg_moderation__config_state s WHERE w.user_id = 41 AND s.id = 1`).Row().Scan(&level, &note, &done); err != nil ||
		level != LevelEnforce || note != "in progress" || !done || !p.parityDone.Load() {
		t.Fatalf("entry = %s %q, converted = %v (%v)", level, note, done, err)
	}
}

package moderation

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// Freeze automatic banning after COUNT but before INSERT. The old code lets
// manual unblock finish here, then resurrects the block from that old count.
// The per-user transaction now orders the whole decision before the unblock.
func TestUnblockCannotBeOvertakenByAnOldAutoBan(t *testing.T) {
	settings := map[string]any{"mode": ModeEnforce, "ban_threshold": 2, "ban_window_seconds": 3600, "ban_duration_hours": 0}
	p, _, fh := startDB(t, settings)
	fh2 := pluginsdktest.NewFakeHost()
	fh2.SetDSN(fh.DSNValue, fh.SchemaValue)
	p2, _ := startNode(t, fh2, settings)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	db, err := p.db(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO plg_moderation__blocks(user_id,source,created_at) VALUES (3,'manual',?)`, now.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO plg_moderation__events(user_id,mode,verdict,action,result,created_at)
		VALUES (3,'observe','block','allow','hit',?),(3,'observe','block','allow','hit',?)`, now.Add(-time.Minute), now.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	// A transaction holds one connection, the session of the barrier lock.
	barrier := db.Begin()
	if barrier.Error != nil {
		t.Fatal(barrier.Error)
	}
	defer barrier.Rollback()
	var key int64
	if err := barrier.Raw(`SELECT pg_backend_pid()`).Row().Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := barrier.Exec(`SELECT pg_advisory_lock(?)`, key).Error; err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unpause := func() {
		releaseOnce.Do(func() {
			if err := barrier.Exec(`SELECT pg_advisory_unlock(?)`, key).Error; err != nil {
				t.Errorf("release test barrier: %v", err)
			}
		})
	}
	defer unpause()
	// The advisory lock exists only in this test trigger, not production code.
	if err := db.Exec(fmt.Sprintf(`CREATE FUNCTION pause_auto_ban() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$;
		CREATE TRIGGER pause_auto_ban BEFORE INSERT ON plg_moderation__blocks FOR EACH ROW
		WHEN (NEW.source = 'auto') EXECUTE FUNCTION pause_auto_ban()`, key)).Error; err != nil {
		t.Fatal(err)
	}
	autoDone := make(chan struct{})
	go func() {
		defer close(autoDone)
		p.applyBans(ctx, map[int64]bool{3: true})
	}()
	var autoPID int
	for autoPID == 0 {
		if err := db.Raw(`SELECT coalesce((SELECT pid FROM pg_locks
			WHERE locktype='advisory' AND objid=? AND NOT granted LIMIT 1),0)`, key).Row().Scan(&autoPID); err != nil {
			t.Fatal(err)
		}
		if autoPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	type result struct {
		resp *pluginv1.HTTPResponse
		err  error
	}
	unblockDone := make(chan result, 1)
	go func() {
		resp, err := p2.deleteBlock(ctx, &pluginv1.HTTPRequest{PathParams: map[string]string{"user_id": "3"}})
		unblockDone <- result{resp, err}
	}()
	var unblock *result
	for unblock == nil {
		var waiting bool
		if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE relation='plg_moderation__block_subjects'::regclass AND pid<>? AND cardinality(pg_blocking_pids(pid))>0)`, autoPID).Row().Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-unblockDone:
			unblock = &r
		case <-ctx.Done():
			t.Fatal("unblock neither finished nor waited for the automatic decision")
		case <-time.After(10 * time.Millisecond):
		}
	}
	unpause()
	select {
	case <-autoDone:
	case <-ctx.Done():
		t.Fatal("automatic ban did not finish")
	}
	if unblock == nil {
		r := <-unblockDone
		unblock = &r
	}
	if unblock.err != nil || unblock.resp.GetStatus() != 200 {
		t.Fatalf("unblock: %v %v", unblock.resp, unblock.err)
	}
	var blocked bool
	if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM plg_moderation__blocks WHERE user_id=3)`).Row().Scan(&blocked); err != nil || blocked {
		t.Fatalf("completed unblock was undone: blocked=%v err=%v", blocked, err)
	}
	// A late event batch from another node must also observe the new baseline.
	p.applyBans(ctx, map[int64]bool{3: true})
	if err := db.Raw(`SELECT EXISTS(SELECT 1 FROM plg_moderation__blocks WHERE user_id=3)`).Row().Scan(&blocked); err != nil || blocked {
		t.Fatalf("old events re-banned the user: blocked=%v err=%v", blocked, err)
	}
}

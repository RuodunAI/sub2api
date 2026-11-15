package moderation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

type cacheReloadHost struct {
	pluginsdk.Host
	db *gorm.DB
}

func (h *cacheReloadHost) DB(ctx context.Context) (*gorm.DB, error) {
	return h.db.WithContext(ctx), nil
}

type reloadBarrier struct {
	armed            atomic.Bool
	entered, release chan struct{}
}

// hold runs after the block reload's SELECT: its snapshot is taken, but the
// plugin has not read or published its rows.
func (b *reloadBarrier) hold(db *gorm.DB) {
	if strings.Contains(db.Statement.SQL.String(), "SELECT user_id, expires_at FROM blocks") && b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.release
	}
}

func TestBlockCacheRejectsLateReload(t *testing.T) {
	for _, cancelOld := range []bool{false, true} {
		name := "newer_snapshot"
		if cancelOld {
			name = "cancelled_snapshot"
		}
		t.Run(name, func(t *testing.T) {
			dsn, schema := pluginsdktest.NewSchema(t, "plg_moderation_cache")
			pluginsdktest.ApplyMigrations(t, dsn, schema, filepath.Join("..", "..", "migrations"))
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			barrier := &reloadBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			db, err := pgdb.Open(dsn, pgdb.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pgdb.Close(db) }()
			if err := db.Callback().Row().After("gorm:row").Register("test:reload_barrier", barrier.hold); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(func() { close(barrier.release) }) }
			defer release()
			if err := db.Exec(`INSERT INTO blocks(user_id,source) VALUES (3,'manual')`).Error; err != nil {
				t.Fatal(err)
			}
			p := New()
			p.host = &cacheReloadHost{db: db}
			oldCtx, stopOld := context.WithCancel(ctx)
			defer stopOld()
			barrier.armed.Store(true)
			oldDone := make(chan error, 1)
			go func() { oldDone <- p.reloadBlocks(oldCtx) }()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				t.Fatal("old SELECT did not reach the publication barrier")
			}
			if cancelOld {
				stopOld()
			} else {
				if err := db.Exec(`UPDATE blocks SET user_id=4 WHERE user_id=3`).Error; err != nil {
					t.Fatal(err)
				}
				if err := p.reloadBlocks(ctx); err != nil {
					t.Fatal(err)
				}
				if !(p.isBlocked(4) && !p.isBlocked(3)) {
					t.Fatal("new reload did not publish the updated state")
				}
			}
			release()
			var oldErr error
			select {
			case oldErr = <-oldDone:
			case <-ctx.Done():
				t.Fatal("old reload did not finish")
			}
			if cancelOld {
				if !errors.Is(oldErr, context.Canceled) || !(p.blockedCount() == 0) {
					t.Fatalf("cancelled reload changed cache: err=%v", oldErr)
				}
			} else if oldErr != nil || !(p.isBlocked(4) && !p.isBlocked(3)) {
				t.Fatalf("old SELECT restored a stale cache after the new reload: err=%v", oldErr)
			}
		})
	}
}

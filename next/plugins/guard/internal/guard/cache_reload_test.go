package guard

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

// afterQuery runs after a GORM query has read its rows. The armed rule
// load stops there until released: read, but not published yet.
func (b *reloadBarrier) afterQuery(tx *gorm.DB) {
	if strings.Contains(tx.Statement.SQL.String(), `FROM "plg_guard__rules"`) && b.armed.CompareAndSwap(true, false) {
		close(b.entered)
		<-b.release
	}
}

func TestRuleCacheRejectsLateReload(t *testing.T) {
	for _, cancelOld := range []bool{false, true} {
		name := "newer_snapshot"
		if cancelOld {
			name = "cancelled_snapshot"
		}
		t.Run(name, func(t *testing.T) {
			dsn, schema := pluginsdktest.NewSchema(t, "plg_guard_cache")
			pluginsdktest.ApplyMigrations(t, dsn, schema, filepath.Join("..", "..", "migrations"))
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			barrier := &reloadBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			db, err := pgdb.Open(dsn, pgdb.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer pgdb.Close(db) //nolint:errcheck // test cleanup
			if err := db.Callback().Query().After("gorm:query").Register("test:reload_barrier", barrier.afterQuery); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(func() { close(barrier.release) }) }
			defer release()
			if err := db.WithContext(ctx).Exec(`UPDATE plg_guard__rules SET pattern='old'`).Error; err != nil {
				t.Fatal(err)
			}
			p := New()
			p.host = &cacheReloadHost{db: db}
			oldCtx, stopOld := context.WithCancel(ctx)
			defer stopOld()
			barrier.armed.Store(true)
			oldDone := make(chan error, 1)
			go func() { oldDone <- p.reloadRules(oldCtx) }()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				t.Fatal("old SELECT did not reach the publication barrier")
			}
			if cancelOld {
				stopOld()
			} else {
				if err := db.WithContext(ctx).Exec(`UPDATE plg_guard__rules SET pattern='new'`).Error; err != nil {
					t.Fatal(err)
				}
				if err := p.reloadRules(ctx); err != nil {
					t.Fatal(err)
				}
				if !(len(p.rules.Load().rules) == 1 && p.rules.Load().rules[0].Pattern == "new") {
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
				if !errors.Is(oldErr, context.Canceled) || !(len(p.rules.Load().rules) == 0) {
					t.Fatalf("cancelled reload changed cache: err=%v", oldErr)
				}
			} else if oldErr != nil || !(len(p.rules.Load().rules) == 1 && p.rules.Load().rules[0].Pattern == "new") {
				t.Fatalf("old SELECT restored a stale cache after the new reload: err=%v", oldErr)
			}
		})
	}
}

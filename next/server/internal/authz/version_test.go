package authz

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// Version is the authz version the node knows: the database's from the
// start, raised by every change it commits or hears of, without a query.
func TestVersion(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	bus := newMemBus()
	s := newService(t, db, bus, nil)
	stored := func() int64 {
		t.Helper()
		var v int64
		if err := db.SQL.QueryRowContext(context.Background(), `SELECT version FROM authz_meta WHERE id = 1`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	start := s.Version()
	if start == 0 || start != stored() {
		t.Fatalf("version after start: %d, stored %d", start, stored())
	}
	// A node starting on a synced database knows the version before any change.
	other := newService(t, db, bus, nil)
	if got := other.Version(); got != start {
		t.Fatalf("second node's version: %d, want %d", got, start)
	}

	uid := addUser(t, s, "v@example.com", RoleUser)
	changed := s.Version()
	if changed <= start || changed != stored() {
		t.Fatalf("version after a role change: %d (was %d, stored %d)", changed, start, stored())
	}
	if got := other.Version(); got != changed {
		t.Fatalf("the other node heard %d, want %d", got, changed)
	}
	set, err := s.PermissionSet(context.Background(), uid)
	if err != nil || set.Version != changed {
		t.Fatalf("permission set version: %d %v, want %d", set.Version, err, changed)
	}
}

// currentVersion is the authz version alone (currentVersions).
func (s *Service) currentVersion(ctx context.Context) (int64, error) {
	v, _, err := s.currentVersions(ctx)
	return v, err
}

// A transaction that bumps twice (a plugin upgrade's commit: ApplyDefaults'
// SyncPlugin, then SetPluginActive) never has the node wait for its own
// first version: no other connection sees it before the commit, and none
// ever does after a rollback, which would leave every decision failing.
func TestBumpTwiceInOneTx(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	uid := addUser(t, s, "twice@example.com", RoleUser)
	start := s.Version()
	rollback := errors.New("rollback")
	err := db.Tx(ctx, func(tx *gorm.DB) error {
		if err := s.SyncPlugin(ctx, tx, "guard", nil); err != nil {
			return err
		}
		if err := s.SetPluginActive(ctx, tx, "guard", true); err != nil {
			return err
		}
		if got := s.Version(); got != start {
			t.Errorf("version inside the transaction: %d, want %d (none committed since)", got, start)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err := s.Can(ctx, uid, "user.read"); err != nil {
		t.Fatalf("a decision after the rollback: %v", err)
	}
}

// A version another transaction committed is taken in by the next bump,
// whether the transaction was another node's (nothing pending here) or
// this node's, not announced yet: checks made inside the bumping
// transaction never trust an enforcer behind it.
func TestBumpTakesInCommitted(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	other := newService(t, db, newMemBus(), nil) // another node: its broadcasts never reach s
	ctx := context.Background()
	bumpIn := func(want int64) {
		t.Helper()
		if err := db.Tx(ctx, func(tx *gorm.DB) error {
			if _, err := s.Bump(ctx, tx); err != nil {
				return err
			}
			if got := s.Version(); got != want {
				t.Errorf("version inside the bumping transaction: %d, want %d", got, want)
			}
			return errors.New("rollback")
		}); err == nil {
			t.Fatal("no rollback")
		}
	}

	addUser(t, other, "committed@example.com", RoleUser)
	committed, err := s.currentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Version() >= committed {
		t.Fatalf("s heard of %d before any bump", committed)
	}
	bumpIn(committed)

	// This node's own write, committed without Committed yet: pending
	// still holds it, under another transaction.
	if err := db.Tx(ctx, func(tx *gorm.DB) error {
		var err error
		committed, err = s.Bump(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bumpIn(committed)
}

// A version whose transaction rolled back, committed again by another
// node's transaction (whose broadcast has not arrived), is announced
// without a scope by committedLater, never with the rolled-back one's.
func TestCommittedLaterAfterRollback(t *testing.T) {
	t.Parallel()
	db := testutil.DB(t)
	bus := newMemBus()
	s := newService(t, db, bus, nil)
	other := newService(t, db, newMemBus(), nil) // another node: its broadcasts never reach s
	ctx := context.Background()
	var mu sync.Mutex
	var heard []authzChange
	bus.Subscribe(core.ChannelAuthzChanged, func(payload []byte) {
		if c, ok := parseChange(payload); ok {
			mu.Lock()
			heard = append(heard, c)
			mu.Unlock()
		}
	})

	before, err := s.currentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := before + 1
	if err := db.Tx(ctx, func(tx *gorm.DB) error {
		if err := s.SetPluginActive(ctx, tx, "guard", true); err != nil {
			return err
		}
		return errors.New("rollback")
	}); err == nil {
		t.Fatal("no rollback")
	}
	addUser(t, other, "again@example.com", RoleUser) // commits v, a user write
	if got, err := s.currentVersion(ctx); err != nil || got != v {
		t.Fatalf("stored version %d %v, want %d", got, err, v)
	}
	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(heard) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if want := (authzChange{Version: v}); !reflect.DeepEqual(heard, []authzChange{want}) {
		t.Fatalf("announced %+v, want only %+v (no scope)", heard, want)
	}
}

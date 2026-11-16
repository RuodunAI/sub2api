package control

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSchemaLockSpansScript proves that EnsureSchema's lock and script still
// run as one implicit transaction through GORM and database/sql: the script
// runs with the lock held, waits while another session holds it, and a
// failing statement undoes the ones before it.
func TestSchemaLockSpansScript(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	probe := quoteIdent(fmt.Sprintf("schema_lock_probe_%d", time.Now().UnixNano()))
	if err := s.db(ctx).Exec("CREATE TABLE " + probe + " (step text, pid int, held bigint)").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db(context.Background()).Exec("DROP TABLE IF EXISTS " + probe) })
	// record notes how many advisory locks the session running the script
	// holds at that point; a transaction-scoped lock is gone once the
	// transaction that took it ends.
	record := func(step string) string {
		return fmt.Sprintf("INSERT INTO %s SELECT '%s', pg_backend_pid(), count(*) FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND granted;\n", probe, step)
	}
	held := func(step string) (pid int, locks int64, rows int64) {
		t.Helper()
		if err := s.db(ctx).Raw("SELECT COALESCE(max(pid),0), COALESCE(max(held),0), count(*) FROM "+probe+" WHERE step=?", step).Row().Scan(&pid, &locks, &rows); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("lock held through the script", func(t *testing.T) {
		if err := execLocked(s.db(ctx), record("held")); err != nil {
			t.Fatal(err)
		}
		pid, locks, rows := held("held")
		if rows != 1 || locks != 1 {
			t.Fatalf("script ran with %d advisory locks in %d rows, want the schema lock", locks, rows)
		}
		var left int64
		if err := s.db(ctx).Raw("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=?", pid).Row().Scan(&left); err != nil || left != 0 {
			t.Fatalf("schema lock outlived the script: %d locks, %v", left, err)
		}
	})

	t.Run("script waits for the lock", func(t *testing.T) {
		holder := s.db(ctx).Begin()
		if holder.Error != nil {
			t.Fatal(holder.Error)
		}
		defer holder.Rollback()
		if err := holder.Exec("SELECT pg_advisory_xact_lock(hashtext('sub2api-shell-schema'))").Error; err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- execLocked(s.db(ctx), record("waited")) }()
		select {
		case err := <-done:
			t.Fatalf("script ran while another session held the schema lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		if _, _, rows := held("waited"); rows != 0 {
			t.Fatal("script statements ran before the lock was granted")
		}
		if err := holder.Commit().Error; err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if _, locks, rows := held("waited"); rows != 1 || locks != 1 {
			t.Fatalf("script after the wait: %d rows, %d locks", rows, locks)
		}
	})

	t.Run("failure undoes the script", func(t *testing.T) {
		if err := execLocked(s.db(ctx), record("undone")+"SELECT 1/0;"); err == nil {
			t.Fatal("failing script reported success")
		}
		if _, _, rows := held("undone"); rows != 0 {
			t.Fatal("statements before the failure were committed on their own")
		}
	})
}

// EnsureSchema drops updater.nodes.cpu_percent, which only shells before
// 0.1.134 used.
func TestSchemaDropsNodeCPUPercent(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if err := s.db(ctx).Exec("ALTER TABLE updater.nodes ADD COLUMN IF NOT EXISTS cpu_percent real").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var left int64
	if err := s.db(ctx).Raw("SELECT count(*) FROM information_schema.columns WHERE table_schema='updater' AND table_name='nodes' AND column_name='cpu_percent'").Row().Scan(&left); err != nil || left != 0 {
		t.Fatalf("cpu_percent left: %d %v", left, err)
	}
}

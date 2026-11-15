package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestStopWaitsForOverflowBeforeConnectionAcquired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	poolCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	db, err := store.OpenSized(poolCtx, testutil.DSN(f.db), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Hold every connection so the overflow is registered but cannot insert.
	conns := make([]*sql.Conn, 0, 2)
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range 2 {
		conn, err := db.SQL.Conn(poolCtx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	s := New(db, f.ledger, f.ledger, nil, Options{QueueSize: 1})
	s.Submit(f.record("queued", false))
	s.Submit(f.record("overflow-stop", false))
	done := make(chan struct{})
	go func() { s.Stop(ctx); close(done) }()
	select {
	case <-done:
		t.Fatal("Stop returned before overflow acquired a connection")
	case <-time.After(30 * time.Millisecond):
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	conns = nil
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("overflow not joined")
	}
	if got := f.scalar(`SELECT count(*) FROM usage_logs WHERE request_id='overflow-stop'`); got != "1" {
		t.Fatal(got)
	}
}

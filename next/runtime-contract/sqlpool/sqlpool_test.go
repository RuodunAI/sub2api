package sqlpool

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a driver whose statement "wait" blocks until release is closed,
// the way an UPDATE waits in PostgreSQL for a row lock, and whose other
// statements affect one row; its first badConns statements fail with
// driver.ErrBadConn.
type fake struct {
	entered  chan struct{} // receives when "wait" starts
	release  chan struct{}
	badConns atomic.Int32
	execs    atomic.Int32
}

func newFake() *fake {
	return &fake{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (f *fake) Connect(context.Context) (driver.Conn, error) { return fakeConn{f}, nil }
func (f *fake) Driver() driver.Driver                        { return nil }

type fakeConn struct{ f *fake }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("fake: no Prepare") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }

func (c fakeConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.f.execs.Add(1)
	if c.f.badConns.Add(-1) >= 0 {
		return nil, driver.ErrBadConn
	}
	if query == "wait" {
		c.f.entered <- struct{}{}
		select {
		case <-c.f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return driver.RowsAffected(1), nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

// openFake is a pool of one connection on a fake driver.
func openFake(t *testing.T) (*sql.DB, *fake) {
	f := newFake()
	db := sql.OpenDB(f)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, f
}

// rowsAffectedWhileBlocked runs an UPDATE through exec, lets a statement
// that waits ("wait") take the pool's only connection, and reads the
// UPDATE's RowsAffected: ok is false when the read is still waiting after
// a while. The waiting statement is released before it returns.
func rowsAffectedWhileBlocked(t *testing.T, db *sql.DB, f *fake, exec func(context.Context, string, ...any) (sql.Result, error)) (ok bool) {
	t.Helper()
	ctx := context.Background()
	res, err := exec(ctx, "update")
	if err != nil {
		t.Fatal(err)
	}
	waiter := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, "wait")
		waiter <- err
	}()
	<-f.entered

	read := make(chan int64, 1)
	go func() {
		n, _ := res.RowsAffected()
		read <- n
	}()
	select {
	case n := <-read:
		ok = true
		if n != 1 {
			t.Errorf("RowsAffected = %d, want 1", n)
		}
	case <-time.After(200 * time.Millisecond):
	}
	close(f.release)
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if !ok {
		<-read
	}
	return ok
}

// The race Pool closes, made certain: on DB, the result waits for the
// statement that took its connection; on Pool, it was read before.
func TestExecResultDoesNotWaitForTheNextStatement(t *testing.T) {
	db, f := openFake(t)
	if !rowsAffectedWhileBlocked(t, db, f, New(db).ExecContext) {
		t.Fatal("Pool's RowsAffected waits for the statement that took its connection")
	}

	db, f = openFake(t)
	if rowsAffectedWhileBlocked(t, db, f, db.ExecContext) {
		t.Fatal("database/sql no longer reads RowsAffected under the pooled connection's mutex: Pool may go")
	}
}

// A statement that failed with driver.ErrBadConn was sent nowhere and is
// tried again, as DB.ExecContext does, at most three times.
func TestExecRetriesBadConnections(t *testing.T) {
	db, f := openFake(t)
	f.badConns.Store(2)
	res, err := New(db).ExecContext(context.Background(), "update")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 || f.execs.Load() != 3 {
		t.Fatalf("RowsAffected = %d after %d executions, want 1 after 3", n, f.execs.Load())
	}
	if _, err := res.LastInsertId(); !errors.Is(err, ErrNoLastInsertID) {
		t.Fatalf("LastInsertId: %v", err)
	}

	f.badConns.Store(3)
	f.execs.Store(0)
	if _, err := New(db).ExecContext(context.Background(), "update"); !errors.Is(err, driver.ErrBadConn) || f.execs.Load() != 3 {
		t.Fatalf("after %d executions: %v, want driver.ErrBadConn after 3", f.execs.Load(), err)
	}
}

// GORM finds the *sql.DB and opens transactions through Pool.
func TestPoolExposesTheDB(t *testing.T) {
	db, _ := openFake(t)
	p := New(db)
	if got, err := p.GetDBConn(); got != db || err != nil {
		t.Fatalf("GetDBConn = %p, %v", got, err)
	}
	tx, err := p.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

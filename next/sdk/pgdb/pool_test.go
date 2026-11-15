package pgdb_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/testpg"
)

// An Exec on the pool reads its result before its connection goes back:
// database/sql reads RowsAffected after, under the connection's mutex,
// which the connection's next statement holds while it waits for a row
// lock. GORM reads the result right after the Exec, so the test widens the
// window on the pool GORM runs on: T holds a row lock on one of two
// connections, A's UPDATE runs on the other, B takes it and waits for T's
// lock, and only then is A's result read.
func TestPoolExecDoesNotWaitForRowLocks(t *testing.T) {
	ctx := context.Background()
	db, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pgdb.Close(db) //nolint:errcheck
	table := fmt.Sprintf("pgdb_exec_lock_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := db.Exec(`CREATE TABLE ` + table + ` (id int PRIMARY KEY, n int NOT NULL DEFAULT 0)`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP TABLE ` + table) //nolint:errcheck
	if err := db.Exec(`INSERT INTO ` + table + ` (id) VALUES (1), (2)`).Error; err != nil {
		t.Fatal(err)
	}

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := tx.Exec(`UPDATE ` + table + ` SET n = 1 WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	res, err := db.WithContext(ctx).Statement.ConnPool.ExecContext(ctx, `UPDATE `+table+` SET n = 2 WHERE id = 2`)
	if err != nil {
		t.Fatal(err)
	}
	waiter := make(chan error, 1)
	go func() { waiter <- db.WithContext(ctx).Exec(`UPDATE ` + table + ` SET n = 3 WHERE id = 1`).Error }()

	watch, err := pgdb.Open(testpg.URL(t), pgdb.Options{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pgdb.Close(watch) //nolint:errcheck
	for deadline := time.Now().Add(10 * time.Second); ; {
		var n int
		err := watch.Raw(`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE ?`, "%"+table+"%").Row().Scan(&n)
		if err == nil && n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never waited for T's lock: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	read := make(chan int64, 1)
	go func() {
		n, _ := res.RowsAffected()
		read <- n
	}()
	select {
	case n := <-read:
		if n != 1 {
			t.Errorf("RowsAffected = %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		tx.Rollback() // frees B, and with it the read
		<-read
		t.Fatal("A's RowsAffected waits for B, which waits for T's row lock")
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
}

// Array parses timestamps and intervals in the server's default formats;
// Open pins them over the DSN's, in either spelling, and its options.
func TestArrayScanWhateverTheDateStyle(t *testing.T) {
	at := time.Date(2024, 5, 1, 10, 0, 0, 123456000, time.UTC)
	const every = 26*time.Hour + 3*time.Second
	base := testpg.URL(t)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	for _, dsn := range []string{
		base,
		base + sep + "DateStyle=German&IntervalStyle=iso_8601",
		base + sep + "options=-c%20datestyle%3DSQL,DMY%20-c%20intervalstyle%3Dsql_standard",
	} {
		db, err := pgdb.Open(dsn, pgdb.Options{MaxConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Style string
			At    pgdb.Array[time.Time]
			Every pgdb.Array[time.Duration]
		}
		err = db.Raw(`SELECT current_setting('datestyle') || ' ' || current_setting('intervalstyle') AS style,
			ARRAY[?::timestamptz] AS at, ARRAY[?::interval] AS every`, at, every.String()).Scan(&got).Error
		pgdb.Close(db) //nolint:errcheck
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if got.Style != "ISO, MDY postgres" && got.Style != "ISO, DMY postgres" {
			t.Errorf("settings %q", got.Style)
		}
		if len(got.At) != 1 || !got.At[0].Equal(at) || len(got.Every) != 1 || got.Every[0] != every {
			t.Errorf("read %v %v", got.At, got.Every)
		}
	}
}

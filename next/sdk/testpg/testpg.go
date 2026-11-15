// Package testpg gives tests a PostgreSQL server without any setup.
//
// URL returns TEST_DATABASE_URL when it is set (CI, a tunnel to a shared
// test server). Otherwise it starts - or reuses - a local PostgreSQL run from
// Go with embedded-postgres: the real server binaries, downloaded once
// from Maven Central and cached, not an emulation. `go test ./...` runs each
// package in its own process at the same time, so the processes share one
// server through a file lock instead of starting forty: the data directory
// lives in the user cache directory, initdb runs once, and the server keeps
// running after the tests exit so the next run starts in milliseconds (stop
// it with `go run github.com/Sub2API-Devs/sup2api/next/sdk/testpg/cmd/testpg stop`).
//
// The server is tuned for tests (fsync off, C locale, UTF8, many
// connections) and only listens on 127.0.0.1. Set SUB2API_TESTPG=off to
// skip database tests instead, e.g. offline before the first download.
package testpg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
)

// Version is the PostgreSQL major the production images run (postgres:16).
const Version = embeddedpostgres.V16

const (
	user     = "postgres"
	password = "postgres"
)

var (
	once     sync.Once
	localURL string
	localErr error
)

// ErrOff is Server's answer when SUB2API_TESTPG=off and TEST_DATABASE_URL
// is unset.
var ErrOff = errors.New("SUB2API_TESTPG=off and TEST_DATABASE_URL not set")

// URL returns a superuser DSN of the server tests use, or skips the test
// when SUB2API_TESTPG=off and TEST_DATABASE_URL is unset.
func URL(t testing.TB) string {
	t.Helper()
	once.Do(func() { localURL, localErr = Server(context.Background()) })
	if errors.Is(localErr, ErrOff) {
		t.Skip(localErr.Error())
	}
	if localErr != nil {
		t.Fatalf("local test PostgreSQL: %v (set TEST_DATABASE_URL, or SUB2API_TESTPG=off to skip database tests)", localErr)
	}
	return localURL
}

// Server is URL for programs: TEST_DATABASE_URL when set, else the local
// server, started unless it runs. It keeps running afterwards; see Acquire.
func Server(ctx context.Context) (string, error) {
	url, _, err := Acquire(ctx)
	return url, err
}

// Dir is where the local server keeps its binaries, data and port.
func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "sub2api-testpg", string(Version)), nil
}

// Acquire is Server for a program that leaves no server behind
// (sub2api-plugin pack and db): release stops the local server when Acquire
// started it, and does nothing when TEST_DATABASE_URL is set or the server
// was running already. Call release once the program is done with the DSN.
func Acquire(ctx context.Context) (url string, release func(), err error) {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u, func() {}, nil
	}
	if strings.EqualFold(os.Getenv("SUB2API_TESTPG"), "off") {
		return "", func() {}, ErrOff
	}
	url, started, err := ensure(ctx)
	if err != nil || !started {
		return url, func() {}, err
	}
	return url, func() { _ = Stop() }, nil
}

// Ensure starts the local server unless one is already running and returns
// its superuser DSN.
func Ensure(ctx context.Context) (string, error) {
	url, _, err := ensure(ctx)
	return url, err
}

// ensure is Ensure, also reporting whether it started the server.
func ensure(ctx context.Context) (url string, started bool, err error) {
	dir, err := Dir()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return "", false, fmt.Errorf("lock %s: %w", dir, err)
	}
	defer unlock()

	if port, ok := runningPort(ctx, dir); ok {
		return dsn(port), false, nil
	}
	port, err := freePort()
	if err != nil {
		return "", false, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", false, err
	}
	defer logf.Close()
	pg := embeddedpostgres.NewDatabase(config(dir, port, logf))
	// pg_ctl start -w returns once the server accepts connections and leaves
	// it running in the background after this process exits.
	if err := pg.Start(); err != nil {
		return "", false, fmt.Errorf("start embedded PostgreSQL %s (log: %s): %w", Version, logf.Name(), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(strconv.Itoa(int(port))), 0o600); err != nil {
		return "", false, err
	}
	return dsn(port), true, nil
}

// Stop stops the local server if it is running.
func Stop() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	port, ok := runningPort(context.Background(), dir)
	if !ok {
		return nil
	}
	pg := embeddedpostgres.NewDatabase(config(dir, port, io.Discard))
	return pg.Stop()
}

func config(dir string, port uint32, log io.Writer) embeddedpostgres.Config {
	return embeddedpostgres.DefaultConfig().
		Version(Version).
		Port(port).
		Username(user).
		Password(password).
		Database("postgres").
		// Start wipes RuntimePath on every call: data and binaries live
		// next to it, never inside.
		RuntimePath(filepath.Join(dir, "run")).
		BinariesPath(filepath.Join(dir, "bin")).
		DataPath(filepath.Join(dir, "data")).
		Locale("C").
		Encoding("UTF8").
		StartParameters(map[string]string{
			"listen_addresses":   "127.0.0.1",
			"max_connections":    "1000",
			"shared_buffers":     "128MB",
			"fsync":              "off",
			"synchronous_commit": "off",
			"full_page_writes":   "off",
		}).
		StartTimeout(2 * time.Minute).
		Logger(log)
}

func dsn(port uint32) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/postgres?sslmode=disable", user, password, port)
}

// runningPort returns the recorded port when a server answers on it.
func runningPort(ctx context.Context, dir string) (uint32, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "port"))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, false
	}
	db, err := pgdb.Open(dsn(uint32(n)), pgdb.Options{MaxConns: 1})
	if err != nil {
		return 0, false
	}
	defer pgdb.Close(db) //nolint:errcheck // a probe
	sqlDB, err := db.DB()
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return uint32(n), sqlDB.PingContext(ctx) == nil
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

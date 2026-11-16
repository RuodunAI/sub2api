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
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u, nil
	}
	if strings.EqualFold(os.Getenv("SUB2API_TESTPG"), "off") {
		return "", ErrOff
	}
	return Ensure(ctx)
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
// (sub2api-plugin pack and db). release stops the local server when an
// Acquire started it and no other program uses it: each Acquire counts as a
// user until its release, and a test (URL, Server) or Ensure keeps the
// server running for good. It does nothing when TEST_DATABASE_URL is set.
// Call release once the program is done with the DSN; a program that dies
// first leaves the server running.
func Acquire(ctx context.Context) (url string, release func(), err error) {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u, func() {}, nil
	}
	if strings.EqualFold(os.Getenv("SUB2API_TESTPG"), "off") {
		return "", func() {}, ErrOff
	}
	url, user, err := ensure(ctx, true)
	if err != nil {
		return "", func() {}, err
	}
	return url, func() { _ = leave(user) }, nil
}

// Ensure starts the local server unless one is already running and returns
// its superuser DSN. The server keeps running.
func Ensure(ctx context.Context) (string, error) {
	url, _, err := ensure(ctx, false)
	return url, err
}

// The files next to the server that track its users (Acquire): transient
// exists while the server is one an Acquire started and only Acquires used,
// users holds a file per Acquire not released yet.
const (
	transientFile = "transient"
	usersDir      = "users"
)

// ensure is Ensure. For an Acquire (transient) it registers a user and
// returns its file; otherwise the server is no longer transient.
func ensure(ctx context.Context, transient bool) (url, user string, err error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return "", "", fmt.Errorf("lock %s: %w", dir, err)
	}
	defer unlock()

	port, running := runningPort(ctx, dir)
	if !running {
		if port, err = start(dir); err != nil {
			return "", "", err
		}
		// Users of a server that is gone are gone too.
		if err := os.RemoveAll(filepath.Join(dir, usersDir)); err != nil {
			return "", "", err
		}
		if transient {
			if err := os.WriteFile(filepath.Join(dir, transientFile), nil, 0o600); err != nil {
				return "", "", err
			}
		}
	}
	if !transient {
		if err := os.Remove(filepath.Join(dir, transientFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", err
		}
		return dsn(port), "", nil
	}
	if err := os.MkdirAll(filepath.Join(dir, usersDir), 0o700); err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(filepath.Join(dir, usersDir), strconv.Itoa(os.Getpid())+"-*")
	if err != nil {
		return "", "", err
	}
	return dsn(port), f.Name(), f.Close()
}

// start starts the server in dir and records its port.
func start(dir string) (uint32, error) {
	port, err := freePort()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	pg := embeddedpostgres.NewDatabase(config(dir, port, logf))
	// pg_ctl start -w returns once the server accepts connections and leaves
	// it running in the background after this process exits.
	if err := pg.Start(); err != nil {
		return 0, fmt.Errorf("start embedded PostgreSQL %s (log: %s): %w", Version, logf.Name(), err)
	}
	return port, os.WriteFile(filepath.Join(dir, "port"), []byte(strconv.Itoa(int(port))), 0o600)
}

// leave ends the use of an Acquire and stops a transient server that has no
// other user.
func leave(user string) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(user); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, transientFile)); err != nil {
		return nil // started or used by a test: it keeps running
	}
	if others, err := os.ReadDir(filepath.Join(dir, usersDir)); err != nil || len(others) > 0 {
		return err
	}
	return stop(dir)
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
	return stop(dir)
}

// stop stops the server in dir, under the lock, and forgets its users.
func stop(dir string) error {
	_ = os.Remove(filepath.Join(dir, transientFile))
	_ = os.RemoveAll(filepath.Join(dir, usersDir))
	port, ok := runningPort(context.Background(), dir)
	if !ok {
		return nil
	}
	return embeddedpostgres.NewDatabase(config(dir, port, io.Discard)).Stop()
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

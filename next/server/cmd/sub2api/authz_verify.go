package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinx"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/config"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

const authzVerifyUsage = "usage: sub2api admin authz-verify [--keys] [--key-users N] [--max-diffs N]"

// Exit codes of authz-verify besides 0 (both engines agree on every user
// and key checked) and 2 (usage).
const (
	verifyMismatch   = 1 // a difference, or an error
	verifyIncomplete = 3 // no difference, but users or keys skipped: run it again on a quiet copy
)

// defaultKeyUsers bounds the users --keys asks Enforce about: a check costs
// every policy row (8-230 µs), so all keys for all users of a large site
// would take hours.
const defaultKeyUsers = 1000

// verifySource is what authz-verify compares (authz.Service): the users and
// keys, and each user's sets as both engines resolve them.
type verifySource interface {
	VerifyTargets(ctx context.Context) (users []int64, keys []string, err error)
	CompareEngines(ctx context.Context, uid int64) (authz.EngineComparison, error)
}

// enforceFunc is the matcher path of the Casbin resolver (casbinx.Enforce
// on it): the answer for key and the version it decided at.
type enforceFunc func(ctx context.Context, uid int64, key string, min int64) (bool, int64, error)

// runAuthzVerify is `sub2api admin authz-verify`: every user's effective set
// from the legacy loader against Casbin's (CONTRACTS §99), and with --keys
// every catalog and granted scoped key through Enforce against the legacy
// set's Has, for --key-users users spread over the list. It reads only,
// against the configured database, without Redis; run it on a copy no one
// writes to, or users read while a write lands are skipped.
func runAuthzVerify(args []string) int {
	fs := flag.NewFlagSet("admin authz-verify", flag.ContinueOnError)
	keys := fs.Bool("keys", false, "also ask Enforce every catalog key and granted scoped key, for --key-users users")
	keyUsers := fs.Int("key-users", defaultKeyUsers, "with --keys, the users asked about, evenly spread over all (0: every user)")
	maxDiffs := fs.Int("max-diffs", 20, "differences to print")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), authzVerifyUsage)
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "exit 0: no difference; 1: a difference or an error; 2: usage; 3: no difference, but users or keys were skipped (read while a write landed)")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *maxDiffs < 0 || *keyUsers < 0 {
		fs.Usage()
		return 2
	}
	cfg, err := config.Load(runtime.GOOS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sub2api admin: config:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sub2api admin: database:", err)
		return 1
	}
	defer db.Close()
	az := authz.New(authz.Deps{DB: db})
	enforce := func(ctx context.Context, uid int64, key string, min int64) (bool, int64, error) {
		return casbinx.Enforce(ctx, az.Casbin(), uid, key, min)
	}
	return authzVerify(ctx, az, enforce, verifyOptions{keys: *keys, keyUsers: *keyUsers, maxDiffs: *maxDiffs}, os.Stdout, os.Stderr)
}

type verifyOptions struct {
	keys     bool
	keyUsers int
	maxDiffs int
}

// verifyReport tallies one run.
type verifyReport struct {
	users, skipped, setDiffs            int
	keys, keyUsers, checks, keysSkipped int
	enforceDiffs                        int
	diffs                               []string
}

func (r *verifyReport) diff(max int, format string, args ...any) {
	if len(r.diffs) < max {
		r.diffs = append(r.diffs, fmt.Sprintf(format, args...))
	}
}

// code is the exit code of the run.
func (r *verifyReport) code() int {
	switch {
	case r.setDiffs+r.enforceDiffs > 0:
		return verifyMismatch
	case r.skipped+r.keysSkipped > 0:
		return verifyIncomplete
	}
	return 0
}

// keyStride is every how many users --keys asks about: users/limit rounded
// up, so at most limit of them, spread over the list.
func keyStride(users, limit int) int {
	if limit <= 0 || users <= limit {
		return 1
	}
	return (users + limit - 1) / limit
}

// verifyProgressEvery is every how many users a progress line goes to
// errOut.
const verifyProgressEvery = 1000

// authzVerify compares every user, prints the summary and the first
// differences to out (progress and errors to errOut), and returns the exit
// code (verifyReport.code; 1 on an error).
func authzVerify(ctx context.Context, src verifySource, enforce enforceFunc, o verifyOptions, out, errOut io.Writer) int {
	users, keys, err := src.VerifyTargets(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "sub2api admin authz-verify:", err)
		return verifyMismatch
	}
	r := verifyReport{users: len(users)}
	stride := keyStride(len(users), o.keyUsers)
	if o.keys {
		r.keys = len(keys)
	}
	for i, uid := range users {
		if i > 0 && i%verifyProgressEvery == 0 {
			fmt.Fprintf(errOut, "authz-verify: %d/%d users\n", i, len(users))
		}
		c, err := src.CompareEngines(ctx, uid)
		if err != nil {
			fmt.Fprintf(errOut, "sub2api admin authz-verify: user %d: %v\n", uid, err)
			return verifyMismatch
		}
		switch {
		case c.Skew:
			r.skipped++
			continue
		case c.Mismatch != nil:
			r.setDiffs++
			r.diff(o.maxDiffs, "user %d v%d set: legacy only %v, casbin only %v", uid, c.Legacy.Version, c.Mismatch.LegacyOnly, c.Mismatch.CasbinOnly)
		}
		if !o.keys || i%stride != 0 {
			continue
		}
		r.keyUsers++
		if err := r.enforceKeys(ctx, enforce, uid, c.Legacy, keys, o.maxDiffs); err != nil {
			fmt.Fprintf(errOut, "sub2api admin authz-verify: user %d: %v\n", uid, err)
			return verifyMismatch
		}
	}
	fmt.Fprintf(out, "users: %d, compared %d, skipped %d (version skew), mismatched %d\n", r.users, r.users-r.skipped, r.skipped, r.setDiffs)
	if o.keys {
		fmt.Fprintf(out, "keys: %d, users asked %d, Enforce checks %d, skipped %d (version skew), mismatched %d\n",
			r.keys, r.keyUsers, r.checks, r.keysSkipped, r.enforceDiffs)
	}
	if len(r.diffs) > 0 {
		fmt.Fprintf(out, "first %d differences:\n", len(r.diffs))
		for _, d := range r.diffs {
			fmt.Fprintln(out, "  "+d)
		}
	}
	return r.code()
}

// enforceKeys asks Enforce every key for uid and compares with legacy's Has;
// an answer decided at another version than legacy's is skipped.
func (r *verifyReport) enforceKeys(ctx context.Context, enforce enforceFunc, uid int64, legacy core.PermissionSet, keys []string, maxDiffs int) error {
	for _, k := range keys {
		got, v, err := enforce(ctx, uid, k, legacy.Version)
		if errors.Is(err, casbinx.ErrSkew) || (err == nil && v != legacy.Version) {
			r.keysSkipped++
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		r.checks++
		if want := legacy.Has(k); got != want {
			r.enforceDiffs++
			r.diff(maxDiffs, "user %d v%d enforce %s: legacy %v, casbin %v", uid, legacy.Version, k, want, got)
		}
	}
	return nil
}

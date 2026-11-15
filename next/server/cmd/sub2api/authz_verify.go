package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/config"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

const authzVerifyUsage = "usage: sub2api admin authz-verify [--keys] [--max-diffs N]"

// verifySource is what authz-verify compares (authz.Service): the users and
// keys, and each user's sets as both engines resolve them.
type verifySource interface {
	VerifyTargets(ctx context.Context) (users []int64, keys []string, err error)
	CompareEngines(ctx context.Context, uid int64) (authz.EngineComparison, error)
}

// enforcer is the matcher path of the Casbin resolver.
type enforcer interface {
	Enforce(ctx context.Context, uid int64, key string, min int64) (bool, error)
}

// runAuthzVerify is `sub2api admin authz-verify`: every user's effective set
// from the legacy loader against Casbin's (CONTRACTS §99), and with --keys
// every catalog and granted scoped key through Enforce against the legacy
// set's Has. It reads only, against the configured database, without
// Redis. Exit 1 on any mismatch.
func runAuthzVerify(args []string) int {
	fs := flag.NewFlagSet("admin authz-verify", flag.ContinueOnError)
	keys := fs.Bool("keys", false, "also ask Enforce every catalog key and granted scoped key for every user")
	maxDiffs := fs.Int("max-diffs", 20, "differences to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *maxDiffs < 0 {
		fmt.Fprintln(os.Stderr, authzVerifyUsage)
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
	return authzVerify(ctx, az, az.Casbin(), verifyOptions{keys: *keys, maxDiffs: *maxDiffs}, os.Stdout, os.Stderr)
}

type verifyOptions struct {
	keys     bool
	maxDiffs int
}

// verifyReport tallies one run.
type verifyReport struct {
	users, skipped, setDiffs   int
	keys, checks, enforceDiffs int
	diffs                      []string
}

func (r *verifyReport) diff(max int, format string, args ...any) {
	if len(r.diffs) < max {
		r.diffs = append(r.diffs, fmt.Sprintf(format, args...))
	}
}

// authzVerify compares every user, prints the summary and the first
// differences to out, and returns the exit code: 0 when both engines
// agree, 1 on a difference or an error (printed to errOut).
func authzVerify(ctx context.Context, src verifySource, res enforcer, o verifyOptions, out, errOut io.Writer) int {
	users, keys, err := src.VerifyTargets(ctx)
	if err != nil {
		fmt.Fprintln(errOut, "sub2api admin authz-verify:", err)
		return 1
	}
	r := verifyReport{users: len(users)}
	if o.keys {
		r.keys = len(keys)
	}
	for _, uid := range users {
		c, err := src.CompareEngines(ctx, uid)
		if err != nil {
			fmt.Fprintf(errOut, "sub2api admin authz-verify: user %d: %v\n", uid, err)
			return 1
		}
		switch {
		case c.Skew:
			r.skipped++
			continue
		case c.Mismatch != nil:
			r.setDiffs++
			r.diff(o.maxDiffs, "user %d v%d set: legacy only %v, casbin only %v", uid, c.Legacy.Version, c.Mismatch.LegacyOnly, c.Mismatch.CasbinOnly)
		}
		if !o.keys {
			continue
		}
		for _, k := range keys {
			got, err := res.Enforce(ctx, uid, k, c.Legacy.Version)
			if err != nil {
				fmt.Fprintf(errOut, "sub2api admin authz-verify: user %d, %s: %v\n", uid, k, err)
				return 1
			}
			r.checks++
			if want := c.Legacy.Has(k); got != want {
				r.enforceDiffs++
				r.diff(o.maxDiffs, "user %d v%d enforce %s: legacy %v, casbin %v", uid, c.Legacy.Version, k, want, got)
			}
		}
	}
	fmt.Fprintf(out, "users: %d, compared %d, skipped %d (version skew), mismatched %d\n", r.users, r.users-r.skipped, r.skipped, r.setDiffs)
	if o.keys {
		fmt.Fprintf(out, "keys: %d, Enforce checks %d, mismatched %d\n", r.keys, r.checks, r.enforceDiffs)
	}
	if len(r.diffs) > 0 {
		fmt.Fprintf(out, "first %d differences:\n", len(r.diffs))
		for _, d := range r.diffs {
			fmt.Fprintln(out, "  "+d)
		}
	}
	if r.setDiffs+r.enforceDiffs > 0 {
		return 1
	}
	return 0
}

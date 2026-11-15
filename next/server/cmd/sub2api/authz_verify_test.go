package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// fakeVerify holds each user's legacy set and Casbin's answers.
type fakeVerify struct {
	users  []int64
	keys   []string
	legacy map[int64]core.PermissionSet
	casbin map[int64]core.PermissionSet
	skew   map[int64]bool
	err    error
}

func (f fakeVerify) VerifyTargets(context.Context) ([]int64, []string, error) {
	return f.users, f.keys, f.err
}

func (f fakeVerify) CompareEngines(_ context.Context, uid int64) (authz.EngineComparison, error) {
	c := authz.EngineComparison{Legacy: f.legacy[uid], Casbin: f.casbin[uid], Skew: f.skew[uid]}
	l, cs := c.Legacy.KeyList(), c.Casbin.KeyList()
	if !c.Skew && strings.Join(l, ",") != strings.Join(cs, ",") {
		c.Mismatch = &authz.ShadowMismatch{Kind: authz.MismatchSet, UserID: uid, LegacyOnly: l, CasbinOnly: cs}
	}
	return c, nil
}

// Enforce answers from the Casbin set.
func (f fakeVerify) Enforce(_ context.Context, uid int64, key string, _ int64) (bool, error) {
	return f.casbin[uid].Has(key), nil
}

func newFakeVerify() fakeVerify {
	set := core.PermissionSetOf("gateway:use", "account:read:group/12")
	return fakeVerify{
		users:  []int64{1, 2, 3},
		keys:   []string{"gateway:use", "account:read", "account:read:group/12"},
		legacy: map[int64]core.PermissionSet{1: set, 2: set, 3: {}},
		casbin: map[int64]core.PermissionSet{1: set, 2: set, 3: {}},
		skew:   map[int64]bool{2: true},
	}
}

func verify(t *testing.T, f fakeVerify, o verifyOptions) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := authzVerify(context.Background(), f, f, o, &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestAuthzVerifyAgrees(t *testing.T) {
	code, out := verify(t, newFakeVerify(), verifyOptions{keys: true, maxDiffs: 5})
	want := "users: 3, compared 2, skipped 1 (version skew), mismatched 0\nkeys: 3, Enforce checks 6, mismatched 0\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, output:\n%s\nwant:\n%s", code, out, want)
	}
}

func TestAuthzVerifyReportsDifferences(t *testing.T) {
	f := newFakeVerify()
	f.casbin[3] = core.PermissionSetOf("account:read")
	code, out := verify(t, f, verifyOptions{maxDiffs: 5})
	if code != 1 || !strings.Contains(out, "mismatched 1\n") || !strings.Contains(out, "user 3 v0 set: legacy only [], casbin only [account:read]") ||
		strings.Contains(out, "keys:") {
		t.Fatalf("set difference: exit %d, output:\n%s", code, out)
	}

	// --keys adds the matcher path; differences past --max-diffs are
	// counted, not printed.
	code, out = verify(t, f, verifyOptions{keys: true, maxDiffs: 2})
	if code != 1 || !strings.Contains(out, "keys: 3, Enforce checks 6, mismatched 2\n") || !strings.Contains(out, "first 2 differences:\n") ||
		!strings.Contains(out, "user 3 v0 enforce account:read: legacy false, casbin true") {
		t.Fatalf("enforce differences: exit %d, output:\n%s", code, out)
	}
}

func TestAuthzVerifyError(t *testing.T) {
	f := newFakeVerify()
	f.err = errors.New("no database")
	if code, out := verify(t, f, verifyOptions{}); code != 1 || !strings.Contains(out, "no database") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

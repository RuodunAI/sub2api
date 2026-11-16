package iam

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/audit"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/migrations"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

func TestTransferSuperAdminHTTP(t *testing.T) {
	k := newKeyEnv(t)
	s, ctx := k.s, context.Background()
	const path = "/system/super-admin/transfer"
	rootLogin := mustLogin(t, s, adminEmail, adminPassword)
	root := rootLogin.User.ID
	secret, _ := enableTwoFactor(t, s, root)
	clearLastStep(t, s, root)
	targetLogin := mustLogin(t, s, "key-user@example.com", adminPassword)
	target := k.userID
	body := func(target int64) map[string]any { return map[string]any{"target_user_id": target} }
	verify := func(token string, how map[string]any) (int, map[string]any) {
		in := map[string]any{"scope": core.StepUpSuperAdminTransfer, "context": "POST /api/v1" + path}
		maps.Copy(in, how)
		return k.call(token, "POST", "/me/verify", in)
	}

	// A user without a second factor verifies with the password alone, and
	// is still not the super admin.
	proof := k.proof(t, targetLogin.AccessToken, core.StepUpSuperAdminTransfer, "POST", path, map[string]any{"method": "password", "password": adminPassword})
	if code, out := k.callProof(targetLogin.AccessToken, proof, "POST", path, body(target)); code != 403 || errCode(out) != "permission_denied" {
		t.Fatalf("non-superuser: %d %v", code, out)
	}
	key := k.createKey(t, root, "user:read")
	if code, out := k.call(key.Key, "POST", path, body(target)); code != 403 {
		t.Fatalf("access key: %d %v", code, out)
	}
	code, out := k.call(rootLogin.AccessToken, "POST", path, body(target))
	details, _ := out["error"].(map[string]any)["details"].(map[string]any)
	if code != 403 || errCode(out) != "verification_required" || details["password_required"] != true || details["scope"] != core.StepUpSuperAdminTransfer {
		t.Fatalf("no proof: %d %v", code, out)
	}
	// The password and a second factor, both.
	if code, out := verify(rootLogin.AccessToken, map[string]any{"method": "password", "password": adminPassword}); code != 400 {
		t.Fatalf("password alone: %d %v", code, out)
	}
	if code, out := verify(rootLogin.AccessToken, map[string]any{"method": "totp", "code": codeAt(t, secret, time.Now())}); code != 400 {
		t.Fatalf("code alone: %d %v", code, out)
	}
	if code, out := verify(rootLogin.AccessToken, map[string]any{"method": "totp", "password": adminPassword}); code != 400 {
		t.Fatalf("no code: %d %v", code, out)
	}
	clearLastStep(t, s, root)
	disabled, err := s.CreateUser(ctx, root, CreateUserInput{Email: "disabled@example.com", DisplayName: "D", Password: adminPassword})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.SQL.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	both := func() map[string]any {
		clearLastStep(t, s, root)
		return map[string]any{"method": "totp", "password": adminPassword, "code": codeAt(t, secret, time.Now())}
	}
	proof = k.proof(t, rootLogin.AccessToken, core.StepUpSuperAdminTransfer, "POST", path, both())
	if code, out := k.callProof(rootLogin.AccessToken, proof, "POST", path, body(disabled.ID)); code != 400 {
		t.Fatalf("disabled target: %d %v", code, out)
	}
	proof = k.proof(t, rootLogin.AccessToken, core.StepUpSuperAdminTransfer, "POST", path, both())
	if code, out := k.callProof(rootLogin.AccessToken, proof, "POST", path, body(target)); code != 204 {
		t.Fatalf("transfer: %d %v", code, out)
	}
	for id, want := range map[int64][]string{root: {authz.RoleAdmin}, target: {authz.RoleSuperAdmin, authz.RoleUser}} {
		u, err := s.GetUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got := slices.Sorted(slices.Values(u.Roles))
		if !slices.Equal(got, want) {
			t.Fatalf("user %d roles %v, want %v", id, got, want)
		}
	}
	renews(t, s, rootLogin, false)
	renews(t, s, targetLogin, false)
	var n int
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'superadmin.transfer' AND user_id = $1 AND target_id = $2`,
		root, strconv.FormatInt(target, 10)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
}

// RecoverSuperAdmin leaves the target the only holder from any start: no
// holder or one (casbin_rule_one_superuser allows no more; several are
// TestRecoverSuperAdminBeforeCasbinRule's).
func TestRecoverSuperAdmin(t *testing.T) {
	for _, holders := range []int{0, 1} {
		t.Run(strconv.Itoa(holders), func(t *testing.T) {
			e := setupAdmin(t)
			s, ctx := e.svc, audit.WithSource(context.Background(), "cli")
			root := mustLogin(t, s, adminEmail, adminPassword).User.ID
			other := mustCreateUser(t, s, "other@example.com")
			target := mustCreateUser(t, s, "target@example.com")
			targetLogin := mustLogin(t, s, "target@example.com", adminPassword)
			enableTwoFactor(t, s, target)
			if holders == 0 {
				require.NoError(t, casbinrule.RemoveMembers(s.db.Gorm, authz.RoleSuperAdmin, []int64{root}))
			}
			if _, err := s.db.SQL.ExecContext(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, target); err != nil {
				t.Fatal(err)
			}

			password, err := s.RecoverSuperAdmin(ctx, " Target@Example.com ", RecoverOptions{ResetPassword: true, ResetTwoFactor: true})
			if err != nil {
				t.Fatal(err)
			}
			wantRoles := map[int64][]string{root: {authz.RoleAdmin}, other: {authz.RoleUser}, target: {authz.RoleSuperAdmin, authz.RoleUser}}
			if holders == 0 {
				wantRoles[root] = nil
			}
			for id, want := range wantRoles {
				u, err := s.GetUser(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if got := slices.Sorted(slices.Values(u.Roles)); !slices.Equal(got, want) {
					t.Fatalf("user %d roles %v, want %v", id, got, want)
				}
			}
			if len(password) != 24 {
				t.Fatalf("password %q", password)
			}
			renews(t, s, targetLogin, false)
			if _, err := s.Login(ctx, "target@example.com", adminPassword, testIP); err == nil {
				t.Fatal("the old password still signs in")
			}
			var enrolled bool
			if err := s.db.SQL.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user_totp WHERE user_id = $1)`, target).Scan(&enrolled); err != nil || enrolled {
				t.Fatalf("two-factor kept: %v %v", enrolled, err)
			}
			mustLogin(t, s, "target@example.com", password)
		})
	}
}

// The server CLI runs in its own process without a bus, so a running node's
// status cache still holds the old token_version. A token signed after the
// change carries a newer one: that node reads the status again instead of
// rejecting the new sign-in until statusCacheTTL.
func TestRecoverSuperAdminNewSignInWorksOnARunningNode(t *testing.T) {
	e := setupAdmin(t)
	s, ctx := e.svc, audit.WithSource(context.Background(), "cli")
	root := mustLogin(t, s, adminEmail, adminPassword)
	mustCreateUser(t, s, "target@example.com")
	for _, p := range []*TokenPair{root, mustLogin(t, s, "target@example.com", adminPassword)} {
		if _, err := s.VerifyAccessToken(ctx, p.AccessToken); err != nil { // fills the status cache
			t.Fatal(err)
		}
	}

	cli := New(Deps{DB: s.db, Config: e.cfg, Authz: authz.New(authz.Deps{DB: s.db})}) // as cmd/sub2api/admin.go
	for _, email := range []string{"target@example.com", adminEmail} {
		if _, err := cli.RecoverSuperAdmin(ctx, email, RecoverOptions{}); err != nil {
			t.Fatal(err)
		}
		for _, who := range []string{"target@example.com", adminEmail} {
			p := mustLogin(t, s, who, adminPassword)
			if _, err := s.VerifyAccessToken(ctx, p.AccessToken); err != nil {
				t.Fatalf("after recovering %s, a new sign-in of %s is rejected: %v", email, who, err)
			}
		}
	}
}

// A deleted user keeps their cached entry whatever version a leftover token
// carries: the row is not read on every request (a token's version is above
// anything a missing row could otherwise be cached with).
func TestStatusCacheKeepsADeletedUser(t *testing.T) {
	s, ctx := setupAdmin(t).svc, context.Background()
	uid := mustCreateUser(t, s, "gone@example.com")
	exec := func(q string) {
		t.Helper()
		if _, err := s.db.SQL.ExecContext(ctx, q, uid); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE users SET token_version = token_version + 1 WHERE id = $1`)
	p := mustLogin(t, s, "gone@example.com", adminPassword)
	exec(`UPDATE users SET deleted_at = now() WHERE id = $1`)
	if _, err := s.VerifyAccessToken(ctx, p.AccessToken); err == nil {
		t.Fatal("a deleted user's token is accepted")
	}
	exec(`UPDATE users SET deleted_at = NULL WHERE id = $1`) // behind the cache's back
	if _, err := s.VerifyAccessToken(ctx, p.AccessToken); err == nil {
		t.Fatal("the cached entry of a deleted user was read again")
	}
}

func TestRecoverSuperAdminUnknownUser(t *testing.T) {
	s := setupAdmin(t).svc
	if _, err := s.RecoverSuperAdmin(context.Background(), "nobody@example.com", RecoverOptions{}); !isCode(err, "not_found") {
		t.Fatalf("unknown email: %v", err)
	}
}

func mustCreateUser(t *testing.T, s *Service, email string) int64 {
	t.Helper()
	u, err := s.CreateUser(context.Background(), 0, CreateUserInput{Email: email, DisplayName: "U", Password: adminPassword})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// On a 0.1.285 database with two super admins 0097 stops the upgrade and
// asks for the CLI, which runs before the migrations reach casbin_rule: it
// makes the user the only holder (the other an admin), refusing a disabled
// user and the resets until the core has migrated; the migrations then go
// through, casbin_rule naming the one holder.
func TestRecoverSuperAdminBeforeCasbinRule(t *testing.T) {
	e := setupAdmin(t) // its configuration
	db := testutil.DBThrough(t, "0096_usage_base_cost.sql")
	ctx := audit.WithSource(context.Background(), "cli")
	_, err := db.SQL.ExecContext(ctx, `
INSERT INTO roles (key, name, builtin, superuser) VALUES ('super_admin', '{}', true, true), ('admin', '{}', true, false), ('user', '{}', true, false);
INSERT INTO users (email, password_hash, status) VALUES ('a@x', '', 'active'), ('b@x', '', 'active'), ('c@x', '', 'disabled');
INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u, roles r WHERE r.key = 'super_admin' AND u.email IN ('a@x', 'b@x')`)
	require.NoError(t, err)
	migrate := func() error {
		_, err := store.Migrate(ctx, db, migrations.FS, store.CoreTracker{}, store.MigrateOptions{LockKey: store.CoreMigrationLockKey})
		return err
	}
	require.ErrorContains(t, migrate(), "several super admins")

	cli := New(Deps{DB: db, Config: e.cfg, Authz: authz.New(authz.Deps{DB: db})}) // as cmd/sub2api/admin.go
	for _, c := range []struct {
		email string
		opts  RecoverOptions
	}{{"a@x", RecoverOptions{ResetPassword: true}}, {"a@x", RecoverOptions{ResetTwoFactor: true}}, {"c@x", RecoverOptions{}}} {
		_, err := cli.RecoverSuperAdmin(ctx, c.email, c.opts)
		require.True(t, isCode(err, "conflict"), "%s %+v: %v", c.email, c.opts, err)
	}
	_, err = cli.RecoverSuperAdmin(ctx, "a@x", RecoverOptions{})
	require.NoError(t, err)

	require.NoError(t, migrate())
	id := func(email string) (v int64) {
		require.NoError(t, db.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&v))
		return v
	}
	require.Equal(t, []string{authz.RoleSuperAdmin}, testutil.RoleKeys(t, db, id("a@x")))
	require.Equal(t, []string{authz.RoleAdmin}, testutil.RoleKeys(t, db, id("b@x")))
}

package authz

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// guardFixture is the users the grant and act-on checks below compare,
// by the numbers the cases name them by (0 is the system): 1 holds
// user:read and user:update, 2 is the super admin, 3 holds user:read, 4
// holds user:read, user:update and settings:manage, 5 holds role:assign.
// Each custom role may assign 1's, 3's and 4's roles, so that only what
// the roles grant decides.
func guardFixture(t *testing.T) (*Service, func(int64) int64) {
	t.Helper()
	db := testutil.DB(t)
	s := newService(t, db, newMemBus(), nil)
	ctx := context.Background()
	roles := map[string][]string{
		"ga": {"user:read", "user:update"},
		"gc": {"user:read"},
		"gd": {"user:read", "user:update", "settings:manage"},
		"gk": {"role:assign"},
	}
	for _, k := range []string{"ga", "gc", "gd", "gk"} {
		if _, err := s.CreateRole(ctx, 0, CreateRoleInput{Key: k, Name: lt(k, k), PermissionKeys: roles[k]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"ga", "gc", "gd", "gk"} {
		if _, err := s.SetRoleAssignable(ctx, 0, roleID(t, s, k), AssignableInput{RoleKeys: []string{"ga", "gc", "gd"}}); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[int64]int64{
		1: addUser(t, s, "g1@x.com", "ga"),
		2: addUser(t, s, "g2@x.com", RoleSuperAdmin),
		3: addUser(t, s, "g3@x.com", "gc"),
		4: addUser(t, s, "g4@x.com", "gd"),
		5: addUser(t, s, "g5@x.com", "gk"),
	}
	return s, func(n int64) int64 { return ids[n] }
}

// ids maps id over ns.
func idsOf(id func(int64) int64, ns []int64) []int64 {
	out := make([]int64, len(ns))
	for i, n := range ns {
		out[i] = id(n)
	}
	return out
}

func TestCanGrant(t *testing.T) {
	t.Parallel()
	s, id := guardFixture(t)

	tests := []struct {
		name    string
		actorID int64
		perms   []string
		wantErr bool
	}{
		{
			name:    "superuser can grant anything",
			actorID: 2,
			perms:   []string{"user:read", "user:update", "settings:manage"},
			wantErr: false,
		},
		{
			name:    "can grant subset of own permissions",
			actorID: 1,
			perms:   []string{"user:read"},
			wantErr: false,
		},
		{
			name:    "can grant all own permissions",
			actorID: 1,
			perms:   []string{"user:read", "user:update"},
			wantErr: false,
		},
		{
			name:    "cannot grant permission not held",
			actorID: 3,
			perms:   []string{"user:update"},
			wantErr: true,
		},
		{
			name:    "cannot grant mix of held and not held",
			actorID: 3,
			perms:   []string{"user:read", "user:update"},
			wantErr: true,
		},
		{
			name:    "empty permissions allowed",
			actorID: 1,
			perms:   []string{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CanGrant(context.Background(), id(tt.actorID), tt.perms)
			if (err != nil) != tt.wantErr {
				t.Errorf("CanGrant() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// H1: granting a superuser role needs the superuser flag, which an access
// key never carries, even one of a superuser.
func TestCoversSuperuser(t *testing.T) {
	t.Parallel()
	s, id := guardFixture(t)
	want := core.PermissionSet{Superuser: true, Keys: map[string]struct{}{}}
	keyed := core.WithAccessKey(context.Background(), core.AccessKeyIdentity{UserID: id(2), Permissions: []string{"role:assign"}})
	tests := []struct {
		name    string
		ctx     context.Context
		actorID int64
		wantErr bool
	}{
		{"system", context.Background(), 0, false},
		{"superuser", context.Background(), 2, false},
		{"superuser's access key", keyed, 2, true},
		{"not a superuser", context.Background(), 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.covers(tt.ctx, s.querier(), id(tt.actorID), want, cannotGrant(tt.ctx))
			if (err != nil) != tt.wantErr {
				t.Fatalf("covers() error = %v, wantErr %v", err, tt.wantErr)
			}
			var e *core.Error
			if tt.wantErr && (!errors.As(err, &e) || e.Details["superuser"] != true) {
				t.Fatalf("details = %v", err)
			}
		})
	}
}

func TestCanActOn(t *testing.T) {
	t.Parallel()
	s, id := guardFixture(t)

	tests := []struct {
		name     string
		actorID  int64
		targetID int64
		wantErr  bool
	}{
		{
			name:     "can act on self",
			actorID:  1,
			targetID: 1,
			wantErr:  false,
		},
		{
			// Actor 0 is the system (CLI recovery, the last-super-admin
			// check), as in the audit log.
			name:     "system can act on a superuser",
			actorID:  0,
			targetID: 2,
			wantErr:  false,
		},
		{
			name:     "superuser can act on anyone",
			actorID:  2,
			targetID: 1,
			wantErr:  false,
		},
		{
			name:     "superuser can act on superuser",
			actorID:  2,
			targetID: 2,
			wantErr:  false,
		},
		{
			name:     "cannot act on superuser",
			actorID:  1,
			targetID: 2,
			wantErr:  true,
		},
		{
			name:     "can act on user with subset of permissions",
			actorID:  1,
			targetID: 3,
			wantErr:  false,
		},
		{
			name:     "can act on user with same permissions",
			actorID:  1,
			targetID: 1,
			wantErr:  false,
		},
		{
			name:     "cannot act on user with superset of permissions",
			actorID:  3,
			targetID: 1,
			wantErr:  true,
		},
		{
			name:     "cannot act on user with additional permissions",
			actorID:  1,
			targetID: 4,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CanActOnUser(context.Background(), id(tt.actorID), core.ActOnIdentity, id(tt.targetID))
			if (err != nil) != tt.wantErr {
				t.Errorf("CanActOnUser() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

var _ core.UserGuard = (*Service)(nil)

func TestCanActOnUsers(t *testing.T) {
	t.Parallel()
	s, id := guardFixture(t)
	keyed := core.WithAccessKey(context.Background(), core.AccessKeyIdentity{UserID: id(2), Permissions: []string{"user:read"}})
	ownKey := core.WithAccessKey(context.Background(), core.AccessKeyIdentity{UserID: id(1), Permissions: []string{"user:read"}})
	bg := context.Background()
	// No step-up verifier is set: a superuser's account through a key is
	// refused for want of a proof; one the request already spent passes.
	const needsProof = "verify it's you to continue"
	spent := core.WithStepUp(keyed, "p", "op")
	core.MarkStepUpSpent(spent, core.StepUpUserSecurityReset, false)
	data, identity := core.ActOnData, core.ActOnIdentity
	// Every custom role here may assign the others and itself: these cases
	// judge what the roles grant and the access key rule (FU6). Peers and
	// oneself under the default lists: TestCanActOnUsersPeerProtection.
	tests := []struct {
		name    string
		ctx     context.Context
		actorID int64
		action  core.UserAction
		targets []int64
		wantErr string // "" = allowed, else the message
	}{
		{"no targets", bg, 1, data, nil, ""},
		{"only itself, within its own grants", bg, 3, data, []int64{3, 3}, ""},
		{"system acts on a superuser", bg, 0, identity, []int64{2, 4}, ""},
		{"all within, duplicates ignored", bg, 1, data, []int64{3, 1, 3}, ""},
		{"one beyond rejects the batch", bg, 1, data, []int64{3, 4}, "you cannot modify a user with higher privileges"},
		{"superuser target", bg, 1, data, []int64{3, 2}, "you cannot modify a superuser"},
		{"superuser acts on anyone", bg, 2, identity, []int64{1, 4, 2}, ""},
		// An access key narrows what its owner may do (the route), not whom.
		{"superuser's key within its keys", keyed, 2, identity, []int64{3}, ""},
		{"superuser's key on a wider user", keyed, 2, identity, []int64{1}, ""},
		{"superuser's key on its owner's data", keyed, 2, data, []int64{2}, ""},
		{"superuser's key on its owner's account", keyed, 2, identity, []int64{2}, needsProof},
		{"superuser's key on its account after a proof", spent, 2, identity, []int64{2}, ""},
		{"another user's key leaves the actor alone", keyed, 3, data, []int64{3}, ""},
		{"a narrower key on its owner", ownKey, 1, identity, []int64{1}, ""},
		{"a narrower key still within its owner's roles", ownKey, 1, data, []int64{4}, "you cannot modify a user with higher privileges"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CanActOnUsers(tt.ctx, id(tt.actorID), tt.action, idsOf(id, tt.targets)...)
			var e *core.Error
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("CanActOnUsers() = %v, want nil", err)
			case tt.wantErr != "" && (!errors.As(err, &e) || e.Message != tt.wantErr):
				t.Fatalf("CanActOnUsers() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestKeysNotIn(t *testing.T) {
	tests := []struct {
		name       string
		want, have []string
		added      []string
	}{
		{"all new", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"none new", []string{"a", "b"}, []string{"b", "a"}, nil},
		{"some new, order kept", []string{"c", "a", "d"}, []string{"a", "b"}, []string{"c", "d"}},
		{"removal only", nil, []string{"a"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keysNotIn(tt.want, tt.have); !slices.Equal(got, tt.added) {
				t.Errorf("keysNotIn(%v, %v) = %v, want %v", tt.want, tt.have, got, tt.added)
			}
		})
	}
}

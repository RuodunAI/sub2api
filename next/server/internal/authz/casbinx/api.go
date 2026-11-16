// Package casbinx decides permissions with Casbin (CONTRACTS §99).
//
// casbin_rule is the one store of the grants and the users' roles; its
// writes are package casbinrule's, in the business transactions. This
// package only reads it, into one Casbin enforcer per node: the p rows of
// active permissions and the superuser row, the g rows of the active users,
// and the virtual role SelfRole linked to every active user. Every decision
// is the enforcer's Enforce through its decision cache, and every list of a
// user's grants its GetImplicitPermissionsForUser.
//
// The enforcer follows the authz versions (authz_meta): a policy write
// (grants, permission statuses, role definitions; policy_version moves)
// makes the node load a new enforcer and drop every decision; a write of
// one user's roles or status (version alone moves, naming the user) makes
// it read that user again, replace the user's g rows in memory and drop
// the user's decisions only. Casbin never writes: the enforcer has no
// adapter and auto-save off, and nothing here calls a Casbin API that
// persists, opens a transaction, watches or reloads by itself (gormguard
// [casbin-api]).
package casbinx

import (
	"context"
	"errors"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// Subjects and the reserved policy values, casbinrule's: the store and the
// enforcer read the rows the same way.
const (
	// SuperKey as the action of a p row grants every key, future ones
	// included: a superuser role's row.
	SuperKey = casbinrule.SuperKey
	// AllRows is the scope of an unscoped grant.
	AllRows = casbinrule.AllRows
	// SelfRole is the virtual role of every active user, holding the self
	// permissions (authz.SelfPermissionKeys); it is never stored, and a
	// stored row of it is not loaded (loadable).
	SelfRole = casbinrule.SelfRole
)

// Rule is one casbin_rule row: p (role subject, action, scope) or g (user
// subject, role subject, ""); v3..v5 are unused.
type Rule struct{ Ptype, V0, V1, V2 string }

// Policy is one consistent read of the policy: the authz and policy
// versions, the status of every catalog action, the p and g rows and the
// active users, all from one read-only repeatable-read transaction.
type Policy struct {
	Version, PolicyVersion int64
	// Active holds the actions whose permission is active; p rows of other
	// actions (disabled or removed plugin permissions) stay stored but are
	// not loaded. SuperKey rows are always loaded.
	Active map[string]bool
	Rules  []Rule
	// Users are the active users (not deleted, status active): only their
	// g rows are loaded, and each is linked to SelfRole.
	Users []int64
}

// UserRoles is one user's g rows as read at Version, under PolicyVersion
// (both of the same read): role subjects, and whether the user is active
// (not deleted, status active). An inactive user holds nothing, not even
// the self permissions.
type UserRoles struct {
	Version, PolicyVersion int64
	Active                 bool
	Roles                  []string
}

// Loader reads the policy from PostgreSQL. Each read sees one snapshot of
// the database and reports its versions.
type Loader interface {
	Policy(ctx context.Context) (Policy, error)
	UserRoles(ctx context.Context, uid int64) (UserRoles, error)
}

// Change is what authz version Version changed, as the node learnt it
// (its own write, authz:changed, the version poll): the users of a write
// of their roles or status, or (Policy set, or no user named) anything.
type Change struct {
	Version int64
	// Policy is the policy version a policy write moved to; 0 when the
	// write changed users only.
	Policy int64
	// Users are the users whose roles or status changed; none means a
	// change the node does not know the extent of.
	Users []int64
}

// UsersOnly reports whether c is known to change the users c.Users alone.
func (c Change) UsersOnly() bool { return c.Policy == 0 && len(c.Users) > 0 }

// Enforcer is what authz.Service calls: the node's Casbin enforcer. Every
// call first waits until the enforcer holds authz version min or newer
// (the newest version the node knows of), at most Options.Wait, and fails
// with ErrStale past it (fail closed).
type Enforcer interface {
	// Enforce is Casbin's Enforce of key for user uid, through the decision
	// cache.
	Enforce(ctx context.Context, uid, min int64, key string) (bool, error)
	// Permissions is uid's grants as Casbin lists them
	// (GetImplicitPermissionsForUser; SuperKey sets Superuser), labelled
	// with the version the enforcer held; its Has is Enforce.
	Permissions(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	// Changed takes in a committed change; the enforcer applies it off the
	// caller.
	Changed(c Change)
}

// ErrStale is the error of a call that could not have an enforcer at the
// version it needs in time: the caller must not decide from an older one.
var ErrStale = errors.New("casbinx: the enforcer does not hold the required authz version")

// Package casbinx resolves effective permissions with Casbin (CONTRACTS
// §99).
//
// casbin_rule is the one store of the grants and the users' roles; its
// writes are package casbinrule's, in the business transactions. This
// package only reads it: one immutable enforcer per policy version
// (snapshot; authz_meta.policy_version, which only the writes of grants,
// permission statuses and role definitions move), built from the p rows of
// active permissions and no g row. A user's roles come with each call,
// read together with the policy version they were read under (RoleCache).
// A write of one user's roles or status moves the authz version only: no
// snapshot is rebuilt for it. Nothing here writes policy, opens a
// transaction of its own or calls the gorm-adapter / Casbin management APIs
// (AddPolicy, SavePolicy, Transaction, TransactionalEnforcer): gormguard
// rejects them.
//
// Requests read the PermissionSet Grants returns and test it with
// PermissionSet.Has (perm.Implies, the same function permMatch registers);
// the matcher itself only answers this package's tests, which check the
// sets against it.
package casbinx

import (
	"context"
	"errors"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// Subjects and the reserved policy values, casbinrule's: the store and the
// resolver read the rows the same way.
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
// versions, the status of every catalog action and the p rows, all from
// one read-only repeatable-read transaction.
type Policy struct {
	Version, PolicyVersion int64
	// Active holds the actions whose permission is active; p rows of other
	// actions (disabled or removed plugin permissions) stay stored but are
	// not loaded. SuperKey rows are always loaded.
	Active map[string]bool
	Rules  []Rule
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

// RoleCache reads the users' roles, through Valkey when configured (shared
// by the nodes) and merging the concurrent reads of one user: Warm returns
// uid's roles read at a version >= min, from Valkey when its entry is at
// exactly min, else from the Loader. A Valkey error skips it (PostgreSQL
// is the truth). The process keeps the resolved sets (authz.Service), not
// the roles.
//
// The snapshots never read the cache themselves: a resolution hands the
// roles Warm returned to the snapshot of their policy version, so the two
// halves of a set cannot come from different policies.
type RoleCache interface {
	Warm(ctx context.Context, uid, min int64) (UserRoles, error)
}

// Resolver is what authz.Service calls. Grants is uid's effective
// permission set as Casbin resolves it at an authz version >= min (the
// set's Version): the user's roles read at that version, and the policy
// snapshot of the policy version they were read under (built if needed,
// blocking, failing closed after a timeout). When the two cannot be had
// under one policy version it fails with ErrSkew.
type Resolver interface {
	Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	// Prewarm starts building the snapshot of policy version policy, off
	// the caller, when the current one is older: the node learnt of a
	// policy write, and the requests that follow should find it built.
	Prewarm(policy int64)
}

// ErrSkew is the error of a resolution whose roles and snapshot were read
// under different policy versions in every round until the snapshot wait
// ran out: policy writes kept landing in between. The caller must not
// combine them (casbin fails closed). A write of a user's roles or status
// does not move the policy version, so it never causes one.
var ErrSkew = errors.New("casbinx: the policy and the user's roles were read under different policy versions")

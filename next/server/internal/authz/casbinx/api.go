// Package casbinx resolves effective permissions with Casbin (CONTRACTS
// §99, Mode P).
//
// The legacy role tables stay the source of truth; casbin_rule is their
// projection, kept in the same transaction by database triggers (migration
// 0100). This package only reads it: one immutable enforcer per authz
// version (snapshot), built from the p rows of active permissions and no g
// row; a user's roles come from the two-level user-role cache (RoleCache)
// at the snapshot's version, and the read-only role manager links each
// role subject to itself only. Nothing here writes policy, opens a transaction of its own or
// calls the gorm-adapter / Casbin management APIs (AddPolicy, SavePolicy,
// Transaction, TransactionalEnforcer): gormguard rejects them.
//
// Hot paths read the PermissionSet Grants returns and test it with
// PermissionSet.Has (perm.Implies, the same function permMatch registers);
// Enforce runs every policy through the matcher (8-230 µs) and is for the
// shadow comparison and diagnostics only.
package casbinx

import (
	"context"
	"errors"
	"strconv"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// Subjects and the reserved policy values.
const (
	// SuperKey as the action of a p row grants every key, future ones
	// included: the projection of roles.superuser.
	SuperKey = "*"
	// AllRows is the scope of an unscoped grant (never empty: gorm-adapter
	// drops trailing empty columns when it loads a row).
	AllRows = "*"
	// SelfRole is the virtual role of every active user, holding the self
	// permissions (authz.SelfPermissionKeys); it is never stored, and a
	// stored row of it is not loaded (loadable).
	SelfRole = "r:_self"
)

// UserSub is the Casbin subject of a user, RoleSub of a role.
func UserSub(id int64) string { return "u:" + strconv.FormatInt(id, 10) }

// RoleSub is the Casbin subject of the role with key key.
func RoleSub(key string) string { return "r:" + key }

// Rule is one casbin_rule row: p (role subject, action, scope) or g (user
// subject, role subject, ""); v3..v5 are unused.
type Rule struct{ Ptype, V0, V1, V2 string }

// Policy is one consistent read of the policy: the authz version, the
// status of every catalog action and the p rows, all from one read-only
// repeatable-read transaction.
type Policy struct {
	Version int64
	// Active holds the actions whose permission is active; p rows of other
	// actions (disabled or removed plugin permissions) stay stored but are
	// not loaded. SuperKey rows are always loaded.
	Active map[string]bool
	Rules  []Rule
}

// UserRoles is one user's g rows as read at Version: role subjects, and
// whether the user is active (not deleted, status active). An inactive user
// holds nothing, not even the self permissions.
type UserRoles struct {
	Version int64
	Active  bool
	Roles   []string
}

// Loader reads the policy from PostgreSQL. Both reads see one snapshot
// (read-only repeatable read) and report its authz version.
type Loader interface {
	Policy(ctx context.Context) (Policy, error)
	UserRoles(ctx context.Context, uid int64) (UserRoles, error)
}

// RoleCache is the two-level user-role cache: the process (L1) and Valkey
// (L2, shared by the nodes; optional). Warm returns uid's roles read at a
// version >= min: from L1, from L2 when its entry is at exactly min, or
// from the Loader. A Valkey error skips L2 (PostgreSQL is the truth).
//
// The snapshots never read the cache themselves: a resolution hands the
// roles Warm returned to the snapshot of the same version, so the two
// halves of a set cannot come from different versions.
type RoleCache interface {
	Warm(ctx context.Context, uid, min int64) (UserRoles, error)
}

// Resolver is what authz.Service calls. Grants is uid's effective
// permission set as Casbin resolves it at a version >= min: the policy
// snapshot (built if needed, blocking, failing closed after a timeout) and
// the user's roles, both read at the set's Version. When they cannot be
// had at one version it fails with ErrSkew.
//
// Its matcher path is the package function Enforce, not a method: an
// interface declared elsewhere cannot take it over, so gormguard
// [casbin-api] sees every call.
type Resolver interface {
	Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	// Prewarm starts building the snapshot of version min, off the caller,
	// when the current one is older: the node learnt of a new version, and
	// the requests that follow should find it built.
	Prewarm(min int64)

	enforce(ctx context.Context, uid int64, key string, min int64) (bool, int64, error)
}

// ErrSkew is the error of a resolution whose snapshot and roles were read
// at different versions, also after one more read of each at the newer of
// the two: writes keep landing in between. The caller must not combine
// them (casbin fails closed; the shadow comparison skips).
var ErrSkew = errors.New("casbinx: the policy and the user's roles were read at different versions")

// Enforce runs key through the matcher for uid, at version min or newer,
// and returns the version it decided at (the same as Grants would label
// its set). It runs every policy row through the matcher (8-230 µs at 400
// rows): for the shadow comparison and diagnostics only; hot paths ask
// Grants' PermissionSet.Has.
func Enforce(ctx context.Context, r Resolver, uid int64, key string, min int64) (ok bool, version int64, err error) {
	return r.enforce(ctx, uid, key, min)
}

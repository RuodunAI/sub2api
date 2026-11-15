// Package casbinx resolves effective permissions with Casbin (CONTRACTS
// §99, Mode P).
//
// The legacy role tables stay the source of truth; casbin_rule is their
// projection, kept in the same transaction by database triggers (migration
// 0100). This package only reads it: one immutable enforcer per authz
// version (Snapshot), built from the p rows of active permissions, with g
// answered by a read-only role manager over the two-level user-role cache
// (RoleCache). Nothing here writes policy, opens a transaction of its own or
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
	// permissions (authz.SelfPermissionKeys); it is never stored.
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

// RoleSource answers g from memory, without I/O: the roles of a user
// subject, ok false when it holds no entry (the role manager then answers
// false: fail closed).
type RoleSource interface {
	Roles(sub string) (roles []string, ok bool)
}

// RoleCache is the two-level user-role cache: the process (L1, read by the
// role manager) and Valkey (L2, shared by the nodes; optional). Warm makes
// sure L1 holds uid's roles read at a version >= min, from L1, L2 or the
// Loader, and returns them. A Valkey error skips L2 (PostgreSQL is the
// truth); an entry is only used at an exact or newer version than min.
type RoleCache interface {
	RoleSource
	Warm(ctx context.Context, uid, min int64) (UserRoles, error)
}

// Resolver is what authz.Service calls. Grants is uid's effective
// permission set as Casbin resolves it at a version >= min: it ensures the
// snapshot (blocking, fail closed after a timeout) and warms the user's
// roles first. The set's Version is the version both were read at; when
// the snapshot and the roles disagree it is the lower of the two, so a
// comparison against the legacy loader can tell a version skew.
type Resolver interface {
	Grants(ctx context.Context, uid, min int64) (core.PermissionSet, error)
	// Enforce is the matcher path for one key (shadow sampling and
	// diagnostics; never a hot path).
	Enforce(ctx context.Context, uid int64, key string, min int64) (bool, error)
}

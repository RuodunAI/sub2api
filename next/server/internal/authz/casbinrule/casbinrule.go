// Package casbinrule is the one reader and writer of casbin_rule, the
// Casbin policy and the only store of who holds which grant (CONTRACTS
// §99, migration 0100):
//
//	p, r:<role key>, <permission key>, *          a grant of every row
//	p, r:<role key>, <permission key>, <scope>    a scoped grant
//	p, r:<role key>, *, *                         a superuser role
//	g, u:<user id>, r:<role key>, ''              a user's role
//
// Every statement naming the table, its ptype or its columns is here, so
// that the mapping above is spelled out once: callers (authz, iam's user
// listing, casbinx's loader) pass role keys, user ids, permission keys,
// scope suffixes and Filters, never subjects nor SQL on this package's
// aliases. Writes run on the caller's handle, inside the business
// transaction that also bumps the authz version (authz.Service.Bump takes
// the authz lock first); nothing here goes through gorm-adapter or
// Casbin's management API, which change memory before the database and
// know nothing of the grant checks.
//
// Every write of a role's p rows also moves that role's revision
// (roles.revision, ReviseChanged) in the same statement, so that a role editor
// holding an older revision is refused (authz, version_conflict) whoever
// changed the role's grants: an editor, a plugin consent, the core sync, a
// scope clear.
//
// The table has no foreign keys (a row names its role and permission by
// key, its user by id): deleting a role, a permission or a plugin clears
// their rows here, in the same transaction (DeleteRole, DropPermissions).
package casbinrule

import (
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Table is the policy table, in gorm-adapter's column layout.
const Table = "casbin_rule"

// The reserved policy values.
const (
	// SuperKey as the action of a p row grants every key, future ones
	// included: a superuser role's row.
	SuperKey = "*"
	// AllRows is the scope of a grant of every row (never empty:
	// gorm-adapter drops trailing empty columns when it loads a row).
	AllRows = "*"
	// SelfRole is the virtual role of every active user, holding the self
	// permissions; never stored (casbin_rule_shape refuses it).
	SelfRole = rolePrefix + "_self"
)

const (
	ptypePolicy = "p"
	ptypeRole   = "g"
	rolePrefix  = "r:"
	userPrefix  = "u:"
)

// UserSub is the Casbin subject of the user id.
func UserSub(id int64) string { return userPrefix + strconv.FormatInt(id, 10) }

// UserOf is the user id of the Casbin subject sub, false when sub is not a
// user's.
func UserOf(sub string) (int64, bool) {
	s, ok := strings.CutPrefix(sub, userPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

// RoleSub is the Casbin subject of the role keyed key.
func RoleSub(key string) string { return rolePrefix + key }

// subs is sub of each of vals, as one array parameter.
func subs[T any](vals []T, sub func(T) string) store.Strings {
	out := make(store.Strings, len(vals))
	for i, v := range vals {
		out[i] = sub(v)
	}
	return out
}

// scope is the policy scope of a canonical suffix: AllRows for "" (every
// row).
func scope(suffix string) string {
	if suffix == "" {
		return AllRows
	}
	return suffix
}

// scopes is scope of each of suffixes, as one array parameter.
func scopes(suffixes []string) store.Strings { return subs(suffixes, scope) }

// roleRules are the g rows giving each of the users userIDs each of the
// roles keys.
func roleRules(userIDs []int64, keys []string) []model.CasbinRule {
	rows := make([]model.CasbinRule, 0, len(userIDs)*len(keys))
	for _, id := range userIDs {
		for _, k := range keys {
			rows = append(rows, model.CasbinRule{Ptype: ptypeRole, V0: UserSub(id), V1: RoleSub(k)})
		}
	}
	return rows
}

// conflict is the ON CONFLICT target of an insert that skips a row already
// there: idx_casbin_rule only, so that a second super admin
// (casbin_rule_one_superuser) still fails.
var conflict = clause.OnConflict{Columns: []clause.Column{{Name: "ptype"}, {Name: "v0"}, {Name: "v1"}, {Name: "v2"}, {Name: "v3"}, {Name: "v4"}, {Name: "v5"}}, DoNothing: true}

// The mapping in SQL, constants only (gormguard [sql-const]): subjects are
// compared as stored (v0 = 'r:' || key), so that the unique index and
// casbin_rule_v1_idx serve the lookups, and substr(subject, keyStart) reads a
// key or an id back out of one (keyStart is one past rolePrefix and
// userPrefix, TestKeyStart). cr is the p or g row; the exported subqueries
// name what they read of their outer query (r a role, u a user) or of their
// parameters.
const (
	keyStart    = "3"
	conflictSQL = `ON CONFLICT (ptype, v0, v1, v2, v3, v4, v5) DO NOTHING`

	// crSuffix and crGrantKey are the canonical suffix ("" every row) and
	// the canonical permission key of the p row cr.
	crSuffix   = `CASE cr.v2 WHEN '` + AllRows + `' THEN '' ELSE cr.v2 END`
	crGrantKey = `CASE cr.v2 WHEN '` + AllRows + `' THEN cr.v1 ELSE cr.v1 || ':' || cr.v2 END`
	crRoleKey  = `substr(cr.v0, ` + keyStart + `)`
	// roleSubOfKey is the subject of the role whose key is column key.
	roleSubOfKey = `('` + rolePrefix + `' || key)`
	// ptypeIs and roleIs restrict cr to the p rows and the g rows.
	ptypeIs = `cr.ptype = '` + ptypePolicy + `'`
	roleIs  = `cr.ptype = '` + ptypeRole + `'`
)

// ------------------------------------------------------------ reads as SQL

// Grants is a subquery of every grant (the superuser rows aside), one row
// each: role (its key), action (the permission key), scope (the canonical
// suffix, "" every row) and key (the canonical permission key). A
// condition on action is served by casbin_rule_v1_idx.
const Grants = `SELECT ` + crRoleKey + ` AS role, cr.v1 AS action, ` + crSuffix + ` AS scope, ` + crGrantKey + ` AS key
FROM ` + Table + ` cr WHERE ` + ptypeIs + ` AND cr.v1 <> '` + SuperKey + `'`

// GrantsOfRole is Grants of the outer query's role r (r.key), by the
// unique index.
const GrantsOfRole = Grants + ` AND cr.v0 = ('` + rolePrefix + `' || r.key)`

// RolesOfUser is a subquery of the keys of the roles the outer query's user
// u (u.id) holds.
const RolesOfUser = `SELECT substr(cr.v1, ` + keyStart + `) FROM ` + Table + ` cr WHERE ` + roleIs + ` AND cr.v0 = ('` + userPrefix + `' || u.id)`

// MembersOfRole is a subquery of the ids of the users holding the outer
// query's role r (r.key), soft-deleted ones included.
const MembersOfRole = `SELECT substr(cr.v0, ` + keyStart + `)::bigint FROM ` + Table + ` cr WHERE ` + roleIs + ` AND cr.v1 = ('` + rolePrefix + `' || r.key)`

// RoleSubsOfUID is a subquery of the role subjects (v1, as stored) of the
// user whose id is the named parameter @uid: what casbinx reads into its
// enforcer when the user's roles or status change.
const RoleSubsOfUID = `SELECT cr.v1 FROM ` + Table + ` cr WHERE ` + roleIs + ` AND cr.v0 = ('` + userPrefix + `' || CAST(@uid AS bigint))`

// ------------------------------------------------------------ reads

// RoleKeysOf selects the distinct keys of the roles userIDs hold, sorted.
func RoleKeysOf(db *gorm.DB, userIDs []int64) *gorm.DB {
	return db.Raw(`SELECT DISTINCT substr(cr.v1, `+keyStart+`) FROM `+Table+` cr WHERE `+roleIs+` AND cr.v0 = ANY(?) ORDER BY 1`, subs(userIDs, UserSub))
}

// LockMembers returns the ids of the users holding the role key, sorted,
// and locks their rows.
func LockMembers(db *gorm.DB, key string) ([]int64, error) {
	ids := []int64{}
	err := db.Raw(`SELECT substr(cr.v0, `+keyStart+`)::bigint FROM `+Table+` cr WHERE `+roleIs+` AND cr.v1 = ? ORDER BY 1 FOR UPDATE`, RoleSub(key)).Scan(&ids).Error
	return ids, err
}

// ReviseChanged moves the revision of the roles whose ids the CTE changed
// returns (column role_id): appended to the CTEs of a write of p rows.
const ReviseChanged = `UPDATE roles SET revision = revision + 1 WHERE id IN (SELECT role_id FROM changed)`

// DeleteScopedSQL deletes the grants whose scope matches one of the regular
// expressions of its one parameter (text[]) and returns role_id and key
// (canonical) of each: (*authz.Service).ClearScopeGrants audits them and
// moves the versions and the roles' revisions (ReviseChanged, the CTE
// named changed) in the same statement.
const DeleteScopedSQL = `DELETE FROM ` + Table + ` cr USING roles r
    WHERE ` + ptypeIs + ` AND cr.v2 ~ ANY(?) AND r.key = ` + crRoleKey + `
    RETURNING r.id AS role_id, ` + crGrantKey + ` AS key`

// ------------------------------------------------------------ grants

// Grant is one p row: role and permission by key, the scope by its
// canonical suffix ("" every row).
type Grant struct {
	Role, Action, Suffix string
}

// Added is a grant a write added or removed: the role's id and the
// canonical permission key, for the audit.
type Added struct {
	RoleID int64  `gorm:"column:role_id"`
	Key    string `gorm:"column:key"`
}

// Filter narrows a grant write to the grants of the permissions, and
// roles, it names; the zero Filter passes every one.
type Filter struct {
	Source      string   // the permission's source ("core", "plugin"); "" any
	Plugin      string   // the permission's plugin key; "" any
	Action      string   // the permission's key; "" any
	Skip        []string // permission statuses left alone ("staged", "removed")
	NoSuperuser bool     // leave the superuser role alone
}

// filterSQL is a Filter as an SQL condition on the role r and the
// permission p of a write's statement: an empty field, or no Skip, passes
// every row.
const filterSQL = `(CAST(? AS text) = '' OR p.source = ?) AND (CAST(? AS text) = '' OR p.plugin_key = ?)
    AND (CAST(? AS text) = '' OR p.key = ?) AND p.status <> ALL(COALESCE(CAST(? AS text[]), '{}'))
    AND NOT (CAST(? AS boolean) AND r.superuser)`

// cond is f as filterSQL.
func (f Filter) cond() clause.Expr {
	return gorm.Expr(filterSQL, f.Source, f.Source, f.Plugin, f.Plugin, f.Action, f.Action, store.Strings(f.Skip), f.NoSuperuser)
}

// AddGrants adds the grants of roles and permissions that exist and pass
// f, each unless the role holds it already or, scoped, holds the action
// for every row (which implies it), and returns what it added by role id
// and key; the roles it added to move their revision.
func AddGrants(db *gorm.DB, grants []Grant, f Filter) ([]Added, error) {
	if len(grants) == 0 {
		return []Added{}, nil
	}
	roles, actions, suffixes := make(store.Strings, len(grants)), make(store.Strings, len(grants)), make([]string, len(grants))
	for i, g := range grants {
		roles[i], actions[i], suffixes[i] = g.Role, g.Action, g.Suffix
	}
	return collectAdded(db.Raw(`
WITH g AS (
    SELECT DISTINCT r.id AS role_id, ('`+rolePrefix+`' || r.key)::text AS v0, p.key::text AS v1, t.v2
    FROM unnest(?::text[], ?::text[], ?::text[]) AS t(role, action, v2)
    JOIN roles r ON r.key = t.role
    JOIN permissions p ON p.key = t.action
    WHERE ?),
ins AS (
    INSERT INTO `+Table+` (ptype, v0, v1, v2)
    SELECT '`+ptypePolicy+`', v0, v1, v2 FROM g
    WHERE v2 = '`+AllRows+`' OR NOT EXISTS (SELECT 1 FROM `+Table+` f
        WHERE f.ptype = '`+ptypePolicy+`' AND f.v0 = g.v0 AND f.v1 = g.v1 AND f.v2 = '`+AllRows+`')
    `+conflictSQL+`
    RETURNING v0, v1, v2),
changed AS (
    SELECT cr.role_id, `+crGrantKey+` AS key
    FROM ins JOIN g cr ON cr.v0 = ins.v0 AND cr.v1 = ins.v1 AND cr.v2 = ins.v2),
revised AS (`+ReviseChanged+`)
SELECT role_id, key FROM changed ORDER BY 1, 2`, roles, actions, scopes(suffixes), f.cond()))
}

// DropGrants deletes the grants of the role key that pass f but those of
// keep (grants of the role), moving its revision when any went. The
// superuser row names no permission and stays.
func DropGrants(db *gorm.DB, key string, f Filter, keep []Grant) error {
	actions, suffixes := make(store.Strings, len(keep)), make([]string, len(keep))
	for i, g := range keep {
		actions[i], suffixes[i] = g.Action, g.Suffix
	}
	return db.Exec(`WITH changed AS (
    DELETE FROM `+Table+` cr USING permissions p, roles r
    WHERE cr.ptype = ? AND cr.v0 = ? AND p.key = cr.v1 AND r.key = ? AND ?
      AND (cr.v1, cr.v2) NOT IN (SELECT * FROM unnest(?::text[], ?::text[]))
    RETURNING r.id AS role_id)
`+ReviseChanged,
		ptypePolicy, RoleSub(key), key, f.cond(), actions, scopes(suffixes)).Error
}

// RevokeScopes deletes the grants of the permission action at the
// canonical suffixes and returns them by role id and key; those roles move
// their revision.
func RevokeScopes(db *gorm.DB, action string, suffixes []string) ([]Added, error) {
	return collectAdded(db.Raw(`WITH changed AS (
    DELETE FROM `+Table+` cr USING roles r
    WHERE cr.ptype = ? AND cr.v1 = ? AND cr.v2 = ANY(?) AND r.key = `+crRoleKey+`
    RETURNING r.id AS role_id, `+crGrantKey+` AS key),
revised AS (`+ReviseChanged+`)
SELECT role_id, key FROM changed`, ptypePolicy, action, scopes(suffixes)))
}

func collectAdded(q *gorm.DB) ([]Added, error) {
	var out []Added
	err := q.Scan(&out).Error
	if out == nil {
		out = []Added{}
	}
	return out, err
}

// DropPermissions deletes every grant of the permissions actions, moving
// the revision of the roles that held one: they are being deleted (a
// plugin's uninstall, its staged keys discarded), and a key created again
// must not find them.
func DropPermissions(db *gorm.DB, actions []string) error {
	if len(actions) == 0 {
		return nil
	}
	return db.Exec(`WITH gone AS (
    DELETE FROM `+Table+` WHERE ptype = ? AND v1 = ANY(?) RETURNING v0)
UPDATE roles SET revision = revision + 1 WHERE `+roleSubOfKey+` IN (SELECT v0 FROM gone)`, ptypePolicy, store.Strings(actions)).Error
}

// SyncSuperusers makes the superuser rows those of the roles flagged
// superuser and returns how many rows it changed.
func SyncSuperusers(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Raw(`
WITH want AS (SELECT ` + roleSubOfKey + `::text AS v0 FROM roles WHERE superuser),
gone AS (
    DELETE FROM ` + Table + ` WHERE ptype = '` + ptypePolicy + `' AND v1 = '` + SuperKey + `' AND v0 NOT IN (SELECT v0 FROM want)
    RETURNING 1),
added AS (
    INSERT INTO ` + Table + ` (ptype, v0, v1, v2) SELECT '` + ptypePolicy + `', v0, '` + SuperKey + `', '` + AllRows + `' FROM want
    ` + conflictSQL + `
    RETURNING 1)
SELECT (SELECT count(*) FROM gone) + (SELECT count(*) FROM added)`).Scan(&n).Error
	return n, err
}

// DeleteRole deletes the role key's grants and assignments: it is being
// deleted, and a role created again with its key must not find them.
func DeleteRole(db *gorm.DB, key string) error {
	sub := RoleSub(key)
	return db.Where("(ptype = ? AND v0 = ?) OR (ptype = ? AND v1 = ?)", ptypePolicy, sub, ptypeRole, sub).Delete(&model.CasbinRule{}).Error
}

// RenameRoleSQL is what an operator runs to rename the role id, keyed
// from, to the key to: one transaction that takes the authz lock and moves
// the version first, as every authz write does, then renames the role and
// its rows together, since a row names its role by key and nothing else
// follows a rename. The bump (both versions: a rename changes the policy)
// makes running nodes load the policy again: their enforcers still name
// the old subject. The keys are what the
// API accepts ([a-z][a-z0-9_]*) or a built-in's: nothing to quote.
func RenameRoleSQL(id int64, from, to string) string {
	return fmt.Sprintf(`BEGIN; UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1 WHERE id = 1; `+
		`UPDATE %[1]s SET v0 = '%[3]s' WHERE ptype = '%[5]s' AND v0 = '%[2]s'; `+
		`UPDATE %[1]s SET v1 = '%[3]s' WHERE ptype = '%[6]s' AND v1 = '%[2]s'; `+
		`UPDATE roles SET key = '%[4]s' WHERE id = %[7]d; COMMIT;`, Table, RoleSub(from), RoleSub(to), to, ptypePolicy, ptypeRole, id)
}

// ------------------------------------------------------------ assignments

// SetRoles makes the roles keys (existing ones; the caller resolved them)
// the roles of the user userID.
func SetRoles(db *gorm.DB, userID int64, keys []string) error {
	if err := db.Where("ptype = ? AND v0 = ? AND v1 <> ALL(?)", ptypeRole, UserSub(userID), subs(keys, RoleSub)).Delete(&model.CasbinRule{}).Error; err != nil {
		return err
	}
	return insert(db, roleRules([]int64{userID}, keys))
}

// AddMembers gives the role key to the users userIDs.
func AddMembers(db *gorm.DB, key string, userIDs []int64) error {
	return insert(db, roleRules(userIDs, []string{key}))
}

// RemoveMembers takes the role key from the users userIDs.
func RemoveMembers(db *gorm.DB, key string, userIDs []int64) error {
	return db.Where("ptype = ? AND v1 = ? AND v0 = ANY(?)", ptypeRole, RoleSub(key), subs(userIDs, UserSub)).Delete(&model.CasbinRule{}).Error
}

// insert adds rows, skipping those already there.
func insert(db *gorm.DB, rows []model.CasbinRule) error {
	if len(rows) == 0 {
		return nil
	}
	return db.Clauses(conflict).Create(&rows).Error
}

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
// that the mapping above is spelled out once: callers (authz, core's scope
// clearing, iam's user listing, casbinx's loader) pass role keys, user ids,
// permission keys and scope suffixes, never subjects. Writes run on the
// caller's handle, inside the business transaction that also bumps the
// authz version (authz.Service.Bump takes the authz lock first); nothing
// here goes through gorm-adapter or Casbin's management API, which change
// memory before the database and know nothing of the grant checks.
//
// The table has no foreign keys (a row names its role and permission by
// key, its user by id): deleting a role, a permission or a plugin clears
// their rows here, in the same transaction (DeleteRole, DropPermissions).
package casbinrule

import (
	"fmt"
	"strconv"

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

// RoleSub is the Casbin subject of the role keyed key.
func RoleSub(key string) string { return rolePrefix + key }

// UserSubs is UserSub of each of ids, as one array parameter.
func UserSubs(ids []int64) store.Strings {
	out := make(store.Strings, len(ids))
	for i, id := range ids {
		out[i] = UserSub(id)
	}
	return out
}

func roleSubs(keys []string) store.Strings {
	out := make(store.Strings, len(keys))
	for i, k := range keys {
		out[i] = RoleSub(k)
	}
	return out
}

// PolicyRule is the p row of the grant g. Writes go through AddGrants,
// which checks the role and permission exist; PolicyRule is for a fixture
// written as is.
func PolicyRule(g Grant) model.CasbinRule {
	return model.CasbinRule{Ptype: ptypePolicy, V0: RoleSub(g.Role), V1: g.Action, V2: scope(g.Suffix)}
}

// RoleRule is the g row giving the user userID the role key.
func RoleRule(userID int64, key string) model.CasbinRule {
	return model.CasbinRule{Ptype: ptypeRole, V0: UserSub(userID), V1: RoleSub(key)}
}

// scope is the policy scope of a canonical suffix: AllRows for "" (every
// row).
func scope(suffix string) string {
	if suffix == "" {
		return AllRows
	}
	return suffix
}

// conflict is the ON CONFLICT target of an insert that skips a row already
// there: idx_casbin_rule only, so that a second super admin
// (casbin_rule_one_superuser) still fails.
var conflict = clause.OnConflict{Columns: []clause.Column{{Name: "ptype"}, {Name: "v0"}, {Name: "v1"}, {Name: "v2"}, {Name: "v3"}, {Name: "v4"}, {Name: "v5"}}, DoNothing: true}

// The mapping in SQL. Subjects are compared as stored (v0 = 'r:' || key),
// so that the unique index and casbin_rule_v1_idx serve the lookups;
// roleKeyOf and userIDOf read a key or an id back out of a subject.
const (
	conflictSQL = `ON CONFLICT (ptype, v0, v1, v2, v3, v4, v5) DO NOTHING`
	// suffixSQL and grantKeySQL read a p row cr: its canonical suffix (""
	// every row) and its canonical permission key.
	suffixSQL   = `CASE cr.v2 WHEN '` + AllRows + `' THEN '' ELSE cr.v2 END`
	grantKeySQL = `CASE cr.v2 WHEN '` + AllRows + `' THEN cr.v1 ELSE cr.v1 || ':' || cr.v2 END`
)

func roleSubSQL(key string) string { return "('" + rolePrefix + "' || " + key + ")" }
func userSubSQL(id string) string  { return "('" + userPrefix + "' || " + id + ")" }
func roleKeyOf(col string) string {
	return "substr(" + col + ", " + strconv.Itoa(len(rolePrefix)+1) + ")"
}
func userIDOf(col string) string {
	return "substr(" + col + ", " + strconv.Itoa(len(userPrefix)+1) + ")::bigint"
}

// ------------------------------------------------------------ reads as SQL

// Grants is a subquery of every grant (the superuser rows aside), one row
// each: role (its key), action (the permission key), scope (the canonical
// suffix, "" every row) and key (the canonical permission key). A
// condition on action is served by casbin_rule_v1_idx.
var Grants = `SELECT ` + roleKeyOf("cr.v0") + ` AS role, cr.v1 AS action, ` + suffixSQL + ` AS scope, ` + grantKeySQL + ` AS key
FROM ` + Table + ` cr WHERE cr.ptype = '` + ptypePolicy + `' AND cr.v1 <> '` + SuperKey + `'`

// GrantsOf is Grants of the role whose key is the SQL expression roleKey
// (a column of an outer query, or a placeholder), by the unique index.
func GrantsOf(roleKey string) string {
	return Grants + ` AND cr.v0 = ` + roleSubSQL(roleKey)
}

// RolesOf is a subquery of the keys of the roles the user whose id is the
// SQL expression userID holds.
func RolesOf(userID string) string {
	return `SELECT ` + roleKeyOf("cr.v1") + ` FROM ` + Table + ` cr WHERE cr.ptype = '` + ptypeRole + `' AND cr.v0 = ` + userSubSQL(userID)
}

// MembersOf is a subquery of the ids of the users holding the role whose
// key is the SQL expression roleKey, soft-deleted ones included.
func MembersOf(roleKey string) string {
	return `SELECT ` + userIDOf("cr.v0") + ` FROM ` + Table + ` cr WHERE cr.ptype = '` + ptypeRole + `' AND cr.v1 = ` + roleSubSQL(roleKey)
}

// ------------------------------------------------------------ reads

// RoleKeysOf selects the distinct keys of the roles userIDs hold, sorted.
func RoleKeysOf(db *gorm.DB, userIDs []int64) *gorm.DB {
	return db.Raw(`SELECT DISTINCT `+roleKeyOf("v1")+` FROM `+Table+` WHERE ptype = '`+ptypeRole+`' AND v0 = ANY(?) ORDER BY 1`, UserSubs(userIDs))
}

// UserRoleSubs returns the role subjects of the user uid, sorted: what
// casbinx hands its snapshot.
func UserRoleSubs(db *gorm.DB, uid int64) ([]string, error) {
	subs := []string{}
	err := db.Table(Table).Where("ptype = ? AND v0 = ?", ptypeRole, UserSub(uid)).Order("v1").Pluck("v1", &subs).Error
	return subs, err
}

// LockMembers returns the ids of the users holding the role key, sorted,
// and locks their rows.
func LockMembers(db *gorm.DB, key string) ([]int64, error) {
	ids := []int64{}
	err := db.Raw(`SELECT `+userIDOf("v0")+` FROM `+Table+` WHERE ptype = '`+ptypeRole+`' AND v1 = ? ORDER BY 1 FOR UPDATE`, RoleSub(key)).Scan(&ids).Error
	return ids, err
}

// ScopedGrants are the grants whose scope matches one of the regular
// expressions patterns.
func ScopedGrants(db *gorm.DB, patterns []string) *gorm.DB {
	return db.Table(Table).Where("ptype = ? AND v2 ~ ANY(?)", ptypePolicy, store.Strings(patterns))
}

// DeleteScopedSQL deletes the grants whose scope matches one of the regular
// expressions of its one parameter (text[]) and returns role_id and key
// (canonical) of each: core.ClearScopeGrants audits them and moves the
// version in the same statement.
var DeleteScopedSQL = `DELETE FROM ` + Table + ` cr USING roles r
    WHERE cr.ptype = '` + ptypePolicy + `' AND cr.v2 ~ ANY(?) AND r.key = ` + roleKeyOf("cr.v0") + `
    RETURNING r.id AS role_id, ` + grantKeySQL + ` AS key`

// OrphansSQL selects, as "<ptype> <v0> <v1> <v2>" sorted, the rows naming
// a user, role or permission that does not exist: what a delete path that
// does not clear its rows leaves behind. None should ever be there; tests
// check it after every database test (testutil.DB).
const OrphansSQL = `SELECT concat_ws(' ', cr.ptype, cr.v0, cr.v1, NULLIF(cr.v2, '')) FROM ` + Table + ` cr
WHERE (cr.ptype = '` + ptypePolicy + `' AND (
        NOT EXISTS (SELECT 1 FROM roles r WHERE ('` + rolePrefix + `' || r.key) = cr.v0)
        OR (cr.v1 <> '` + SuperKey + `' AND NOT EXISTS (SELECT 1 FROM permissions p WHERE p.key = cr.v1))))
   OR (cr.ptype = '` + ptypeRole + `' AND (
        NOT EXISTS (SELECT 1 FROM users u WHERE ('` + userPrefix + `' || u.id) = cr.v0)
        OR NOT EXISTS (SELECT 1 FROM roles r WHERE ('` + rolePrefix + `' || r.key) = cr.v1)))
ORDER BY 1`

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

// AddGrants adds the grants of roles and permissions that exist and pass
// cond (an SQL condition on the role r and the permission p), each unless
// the role holds it already or, scoped, holds the action for every row
// (which implies it), and returns what it added by role id and key.
func AddGrants(db *gorm.DB, grants []Grant, cond clause.Expr) ([]Added, error) {
	if len(grants) == 0 {
		return []Added{}, nil
	}
	roles, actions, suffixes := make(store.Strings, len(grants)), make(store.Strings, len(grants)), make(store.Strings, len(grants))
	for i, g := range grants {
		roles[i], actions[i], suffixes[i] = g.Role, g.Action, g.Suffix
	}
	return collectAdded(db.Raw(`
WITH g AS (
    SELECT DISTINCT r.id AS role_id, `+roleSubSQL("r.key")+`::text AS v0, p.key::text AS v1,
        (CASE t.sfx WHEN '' THEN '`+AllRows+`' ELSE t.sfx END)::text AS v2
    FROM unnest(?::text[], ?::text[], ?::text[]) AS t(role, action, sfx)
    JOIN roles r ON r.key = t.role
    JOIN permissions p ON p.key = t.action
    WHERE ?),
ins AS (
    INSERT INTO `+Table+` (ptype, v0, v1, v2)
    SELECT '`+ptypePolicy+`', v0, v1, v2 FROM g
    WHERE v2 = '`+AllRows+`' OR NOT EXISTS (SELECT 1 FROM `+Table+` f
        WHERE f.ptype = '`+ptypePolicy+`' AND f.v0 = g.v0 AND f.v1 = g.v1 AND f.v2 = '`+AllRows+`')
    `+conflictSQL+`
    RETURNING v0, v1, v2)
SELECT g.role_id, CASE g.v2 WHEN '`+AllRows+`' THEN g.v1 ELSE g.v1 || ':' || g.v2 END AS key
FROM ins JOIN g ON g.v0 = ins.v0 AND g.v1 = ins.v1 AND g.v2 = ins.v2
ORDER BY 1, 2`, roles, actions, suffixes, cond))
}

// DropGrants deletes the grants of the role key whose permission p passes
// within (an SQL condition on p) but those of keep (grants of the role).
// The superuser row names no permission and stays.
func DropGrants(db *gorm.DB, key string, within clause.Expr, keep []Grant) error {
	actions, scopes := store.Strings{}, store.Strings{}
	for _, g := range keep {
		actions, scopes = append(actions, g.Action), append(scopes, scope(g.Suffix))
	}
	return db.Exec(`DELETE FROM `+Table+` cr USING permissions p
WHERE cr.ptype = ? AND cr.v0 = ? AND p.key = cr.v1 AND ?
  AND (cr.v1, cr.v2) NOT IN (SELECT * FROM unnest(?::text[], ?::text[]))`,
		ptypePolicy, RoleSub(key), within, actions, scopes).Error
}

// RevokeScopes deletes the grants of the permission action at the
// canonical suffixes and returns them by role id and key.
func RevokeScopes(db *gorm.DB, action string, suffixes []string) ([]Added, error) {
	return collectAdded(db.Raw(`DELETE FROM `+Table+` cr USING roles r
WHERE cr.ptype = ? AND cr.v1 = ? AND cr.v2 = ANY(?) AND r.key = `+roleKeyOf("cr.v0")+`
RETURNING r.id AS role_id, `+grantKeySQL+` AS key`, ptypePolicy, action, store.Strings(suffixes)))
}

func collectAdded(q *gorm.DB) ([]Added, error) {
	var out []Added
	err := q.Scan(&out).Error
	if out == nil {
		out = []Added{}
	}
	return out, err
}

// DropPermissions deletes every grant of the permissions actions: they are
// being deleted (a plugin's uninstall, its staged keys discarded), and a
// key created again must not find them.
func DropPermissions(db *gorm.DB, actions []string) error {
	if len(actions) == 0 {
		return nil
	}
	return db.Where("ptype = ? AND v1 = ANY(?)", ptypePolicy, store.Strings(actions)).Delete(&model.CasbinRule{}).Error
}

// SyncSuperusers makes the superuser rows those of the roles flagged
// superuser and returns how many rows it changed.
func SyncSuperusers(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Raw(`
WITH want AS (SELECT ` + roleSubSQL("key") + `::text AS v0 FROM roles WHERE superuser),
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

// RenameRoleSQL is the statement an operator runs to rename the role id,
// keyed from, to the key to: the role and its rows together, in one
// statement, since a row names its role by key and nothing else follows a
// rename. The keys are what the API accepts ([a-z][a-z0-9_]*) or a
// built-in's: nothing to quote.
func RenameRoleSQL(id int64, from, to string) string {
	return fmt.Sprintf(`WITH p AS (UPDATE %[1]s SET v0 = '%[3]s' WHERE ptype = '%[5]s' AND v0 = '%[2]s'),
g AS (UPDATE %[1]s SET v1 = '%[3]s' WHERE ptype = '%[6]s' AND v1 = '%[2]s')
UPDATE roles SET key = '%[4]s' WHERE id = %[7]d;`, Table, RoleSub(from), RoleSub(to), to, ptypePolicy, ptypeRole, id)
}

// ------------------------------------------------------------ assignments

// SetRoles makes the roles keys (existing ones; the caller resolved them)
// the roles of the user userID.
func SetRoles(db *gorm.DB, userID int64, keys []string) error {
	sub := UserSub(userID)
	if err := db.Where("ptype = ? AND v0 = ? AND v1 <> ALL(?)", ptypeRole, sub, roleSubs(keys)).Delete(&model.CasbinRule{}).Error; err != nil {
		return err
	}
	rows := make([]model.CasbinRule, len(keys))
	for i, k := range keys {
		rows[i] = RoleRule(userID, k)
	}
	return Insert(db, rows)
}

// AddMembers gives the role key to the users userIDs.
func AddMembers(db *gorm.DB, key string, userIDs []int64) error {
	rows := make([]model.CasbinRule, len(userIDs))
	for i, id := range userIDs {
		rows[i] = RoleRule(id, key)
	}
	return Insert(db, rows)
}

// RemoveMembers takes the role key from the users userIDs.
func RemoveMembers(db *gorm.DB, key string, userIDs []int64) error {
	return db.Where("ptype = ? AND v1 = ? AND v0 = ANY(?)", ptypeRole, RoleSub(key), UserSubs(userIDs)).Delete(&model.CasbinRule{}).Error
}

// Insert adds rows (PolicyRule, RoleRule), skipping those already there.
func Insert(db *gorm.DB, rows []model.CasbinRule) error {
	if len(rows) == 0 {
		return nil
	}
	return db.Clauses(conflict).Create(&rows).Error
}

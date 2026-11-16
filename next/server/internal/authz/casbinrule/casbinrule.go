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
// (roles.revision) in the same transaction, so that a role editor
// holding an older revision is refused (authz, version_conflict) whoever
// changed the role's grants: an editor, a plugin consent, the core sync, a
// scope clear.
//
// The table has no foreign keys (a row names its role and permission by
// key, its user by id): deleting a role, a permission or a plugin clears
// their rows here, in the same transaction (DeleteRole, DropPermissions).
package casbinrule

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
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
// name what they read of their outer query (r a role, u a user).
const (
	keyStart = "3"

	// crSuffix and crGrantKey are the canonical suffix ("" every row) and
	// the canonical permission key of the p row cr.
	crSuffix   = `CASE cr.v2 WHEN '` + AllRows + `' THEN '' ELSE cr.v2 END`
	crGrantKey = `CASE cr.v2 WHEN '` + AllRows + `' THEN cr.v1 ELSE cr.v1 || ':' || cr.v2 END`
	crRoleKey  = `substr(cr.v0, ` + keyStart + `)`
	// userIDOfV0 and roleKeyOfV1 read the user id and the role key out of
	// a g row.
	userIDOfV0  = `substr(v0, ` + keyStart + `)::bigint`
	roleKeyOfV1 = `substr(v1, ` + keyStart + `)`
	// roleSubOfKey and roleSubOfR are the subject of the role whose key is
	// column key, and of the role r.
	roleSubOfKey = `('` + rolePrefix + `' || key)`
	roleSubOfR   = `('` + rolePrefix + `' || r.key)`
	// ptypeIs and roleIs restrict cr to the p rows and the g rows.
	ptypeIs = `cr.ptype = '` + ptypePolicy + `'`
	roleIs  = `cr.ptype = '` + ptypeRole + `'`
	// grantColumns and grantRows are Grants' select list and condition.
	grantColumns = crRoleKey + ` AS role, cr.v1 AS action, ` + crSuffix + ` AS scope, ` + crGrantKey + ` AS key`
	grantRows    = ptypeIs + ` AND cr.v1 <> '` + SuperKey + `'`
)

// ------------------------------------------------------------ reads as SQL

// Grants is a subquery of every grant (the superuser rows aside), one row
// each: role (its key), action (the permission key), scope (the canonical
// suffix, "" every row) and key (the canonical permission key). A
// condition on action is served by casbin_rule_v1_idx.
const Grants = `SELECT ` + grantColumns + ` FROM ` + Table + ` cr WHERE ` + grantRows

// GrantsOfRole is Grants of the outer query's role r (r.key), by the
// unique index.
const GrantsOfRole = Grants + ` AND cr.v0 = ` + roleSubOfR

// RolesOfUser is a subquery of the keys of the roles the outer query's user
// u (u.id) holds.
const RolesOfUser = `SELECT substr(cr.v1, ` + keyStart + `) FROM ` + Table + ` cr WHERE ` + roleIs + ` AND cr.v0 = ('` + userPrefix + `' || u.id)`

// MembersOfRole is a subquery of the ids of the users holding the outer
// query's role r (r.key), soft-deleted ones included.
const MembersOfRole = `SELECT substr(cr.v0, ` + keyStart + `)::bigint FROM ` + Table + ` cr WHERE ` + roleIs + ` AND cr.v1 = ` + roleSubOfR

// ------------------------------------------------------------ reads

// GrantRows is Grants as a query, for a builder's FROM or JOIN.
func GrantRows(db *gorm.DB) *gorm.DB {
	return db.Table(Table + ` cr`).Select(grantColumns).Where(grantRows)
}

// RoleKeysOf selects the distinct keys of the roles userIDs hold, sorted.
func RoleKeysOf(db *gorm.DB, userIDs []int64) *gorm.DB {
	return db.Model(&model.CasbinRule{}).Distinct(roleKeyOfV1).Where("ptype = ? AND v0 = ANY(?)", ptypeRole, subs(userIDs, UserSub)).Order("1")
}

// RoleSubsOf selects the role subjects (v1, as stored) of the user uid,
// sorted: what casbinx reads into its enforcer when the user's roles or
// status change.
func RoleSubsOf(db *gorm.DB, uid int64) *gorm.DB {
	return db.Model(&model.CasbinRule{}).Select("v1").Where("ptype = ? AND v0 = ?", ptypeRole, UserSub(uid)).Order("v1")
}

// MembersOf selects the ids of the users holding a role of roles, a query
// of the roles table whose select list it sets; soft-deleted users
// included.
func MembersOf(db, roles *gorm.DB) *gorm.DB {
	return db.Model(&model.CasbinRule{}).Select(userIDOfV0).Where("ptype = ? AND v1 IN (?)", ptypeRole, roles.Select(roleSubOfKey))
}

// Scopes returns the distinct scopes (canonical suffixes, every row aside)
// the permission action is granted at, sorted.
func Scopes(db *gorm.DB, action string) ([]string, error) {
	out := []string{}
	err := db.Model(&model.CasbinRule{}).Distinct("v2").Where("ptype = ? AND v1 = ? AND v2 <> ?", ptypePolicy, action, AllRows).Order("v2").Pluck("v2", &out).Error
	return out, err
}

// LockMembers returns the ids of the users holding the role key, sorted,
// and locks their rows.
func LockMembers(db *gorm.DB, key string) ([]int64, error) {
	if err := store.RequireTx(db); err != nil {
		return nil, err
	}
	ids := []int64{}
	err := db.Model(&model.CasbinRule{}).Select(userIDOfV0).Where("ptype = ? AND v1 = ?", ptypeRole, RoleSub(key)).Order("1").
		Clauses(store.ForUpdate()).Scan(&ids).Error
	return ids, err
}

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

// grantKey is the canonical permission key of the p row (v1, v2).
func grantKey(v1, v2 string) string {
	if v2 == AllRows {
		return v1
	}
	return v1 + ":" + v2
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

// where narrows a query of the role r and the permission p to f, for
// db.Scopes.
func (f Filter) where(db *gorm.DB) *gorm.DB {
	if f.Source != "" {
		db = db.Where("p.source = ?", f.Source)
	}
	if f.Plugin != "" {
		db = db.Where("p.plugin_key = ?", f.Plugin)
	}
	if f.Action != "" {
		db = db.Where("p.key = ?", f.Action)
	}
	if len(f.Skip) > 0 {
		db = db.Where("p.status <> ALL(?)", store.Strings(f.Skip))
	}
	if f.NoSuperuser {
		db = db.Where("NOT r.superuser")
	}
	return db
}

// AddGrants adds the grants of roles and permissions that exist and pass
// f, each unless the role holds it already or, scoped, holds the action
// for every row (which implies it), and returns what it added by role id
// and key; the roles it added to move their revision. It reads what the
// roles hold first, so what it reports rests on the authz lock its
// transaction holds (the caller's Bump), as every grant write's does.
func AddGrants(db *gorm.DB, grants []Grant, f Filter) ([]Added, error) {
	added := []Added{}
	if len(grants) == 0 {
		return added, nil
	}
	roles, actions := make(store.Strings, len(grants)), make(store.Strings, len(grants))
	for i, g := range grants {
		roles[i], actions[i] = g.Role, g.Action
	}
	// Each role and permission of grants that exist and pass f, once per
	// scope the role holds the permission at (once, with none, if none).
	var held []struct {
		RoleID       int64
		Role, Action string
		Scope        *string
	}
	err := db.Table("roles r").Select("r.id AS role_id, r.key AS role, p.key AS action, cr.v2 AS scope").
		Joins("JOIN permissions p ON p.key = ANY(?)", actions).
		Joins("LEFT JOIN "+Table+" cr ON cr.ptype = ? AND cr.v0 = "+roleSubOfR+" AND cr.v1 = p.key", ptypePolicy).
		Where("r.key = ANY(?)", roles).Scopes(f.where).Scan(&held).Error
	if err != nil {
		return nil, err
	}
	type target struct {
		roleID int64
		scopes []string
	}
	found := map[Grant]*target{}
	for _, h := range held {
		t := found[Grant{Role: h.Role, Action: h.Action}]
		if t == nil {
			t = &target{roleID: h.RoleID}
			found[Grant{Role: h.Role, Action: h.Action}] = t
		}
		if h.Scope != nil {
			t.scopes = append(t.scopes, *h.Scope)
		}
	}
	var rows []model.CasbinRule
	revised := map[int64]bool{}
	for _, g := range grants {
		t, row := found[Grant{Role: g.Role, Action: g.Action}], model.CasbinRule{Ptype: ptypePolicy, V0: RoleSub(g.Role), V1: g.Action, V2: scope(g.Suffix)}
		if t == nil || slices.Contains(t.scopes, row.V2) || slices.Contains(t.scopes, AllRows) || slices.Contains(rows, row) {
			continue
		}
		rows = append(rows, row)
		added = append(added, Added{RoleID: t.roleID, Key: grantKey(row.V1, row.V2)})
		revised[t.roleID] = true
	}
	if len(rows) == 0 {
		return added, nil
	}
	if err := insert(db, rows); err != nil {
		return nil, err
	}
	ids := store.Int64s(slices.Collect(maps.Keys(revised)))
	if err := db.Model(&model.Role{}).Where("id = ANY(?)", ids).Update("revision", gorm.Expr("revision + 1")).Error; err != nil {
		return nil, err
	}
	slices.SortFunc(added, func(a, b Added) int { return cmp.Or(cmp.Compare(a.RoleID, b.RoleID), strings.Compare(a.Key, b.Key)) })
	return added, nil
}

// DropGrants deletes the grants of the role key that pass f but those of
// keep (grants of the role), moving its revision when any went. The
// superuser row names no permission and stays.
func DropGrants(db *gorm.DB, key string, f Filter, keep []Grant) error {
	q := db.Where("ptype = ? AND v0 = ?", ptypePolicy, RoleSub(key)).
		Where("v1 IN (?)", db.Table("permissions p").Select("p.key").Joins("JOIN roles r ON r.key = ?", key).Scopes(f.where))
	if len(keep) > 0 {
		pairs := make([]any, len(keep))
		for i, g := range keep {
			pairs[i] = []any{g.Action, scope(g.Suffix)}
		}
		q = q.Not(clause.IN{Column: []clause.Column{{Name: "v1"}, {Name: "v2"}}, Values: pairs})
	}
	gone := q.Delete(&model.CasbinRule{})
	if gone.Error != nil || gone.RowsAffected == 0 {
		return gone.Error
	}
	_, err := revise(db, []string{key})
	return err
}

// RevokeScopes deletes the grants of the permission action at the
// canonical suffixes and returns them by role id and key; those roles move
// their revision.
func RevokeScopes(db *gorm.DB, action string, suffixes []string) ([]Added, error) {
	return deleteGrants(db, db.Where("ptype = ? AND v1 = ? AND v2 = ANY(?)", ptypePolicy, action, scopes(suffixes)))
}

// ClearScopes deletes the grants whose scope matches one of the regular
// expressions patterns and returns them by role id and key; those roles
// move their revision. (*authz.Service).ClearScopeGrants audits them and
// moves the versions.
func ClearScopes(db *gorm.DB, patterns []string) ([]Added, error) {
	return deleteGrants(db, db.Where("ptype = ? AND v2 ~ ANY(?)", ptypePolicy, store.Strings(patterns)))
}

// DropPermissions deletes every grant of the permissions actions, moving
// the revision of the roles that held one: they are being deleted (a
// plugin's uninstall, its staged keys discarded), and a key created again
// must not find them.
func DropPermissions(db *gorm.DB, actions []string) error {
	if len(actions) == 0 {
		return nil
	}
	_, err := deleteGrants(db, db.Where("ptype = ? AND v1 = ANY(?)", ptypePolicy, store.Strings(actions)))
	return err
}

// deleteGrants deletes the p rows q (a condition on db) selects, moves the
// revision of their roles and returns them by role id and key, those of a
// role that exists.
func deleteGrants(db, q *gorm.DB) ([]Added, error) {
	var gone []model.CasbinRule
	if err := q.Clauses(clause.Returning{Columns: []clause.Column{{Name: "v0"}, {Name: "v1"}, {Name: "v2"}}}).Delete(&gone).Error; err != nil {
		return nil, err
	}
	keys := make([]string, len(gone))
	for i, r := range gone {
		keys[i] = strings.TrimPrefix(r.V0, rolePrefix)
	}
	ids, err := revise(db, keys)
	if err != nil {
		return nil, err
	}
	out := make([]Added, 0, len(gone))
	for i, r := range gone {
		if id, ok := ids[keys[i]]; ok {
			out = append(out, Added{RoleID: id, Key: grantKey(r.V1, r.V2)})
		}
	}
	return out, nil
}

// revise moves the revision of the roles keyed keys and returns their ids
// by key.
func revise(db *gorm.DB, keys []string) (map[string]int64, error) {
	ids := map[string]int64{}
	if len(keys) == 0 {
		return ids, nil
	}
	var roles []model.Role
	err := db.Model(&roles).Clauses(clause.Returning{Columns: []clause.Column{{Name: "id"}, {Name: "key"}}}).
		Where("key = ANY(?)", store.Strings(keys)).Update("revision", gorm.Expr("revision + 1")).Error
	for _, r := range roles {
		ids[r.Key] = r.ID
	}
	return ids, err
}

// SyncSuperusers makes the superuser rows those of the roles flagged
// superuser and returns how many rows it changed. It reads them first, so
// it rests on the authz lock its transaction holds (SyncCore's).
func SyncSuperusers(db *gorm.DB) (int64, error) {
	var want, have []string
	if err := db.Model(&model.Role{}).Where("superuser").Pluck(roleSubOfKey, &want).Error; err != nil {
		return 0, err
	}
	if err := db.Model(&model.CasbinRule{}).Where("ptype = ? AND v1 = ?", ptypePolicy, SuperKey).Pluck("v0", &have).Error; err != nil {
		return 0, err
	}
	var n int64
	if stale := slices.DeleteFunc(slices.Clone(have), func(v0 string) bool { return slices.Contains(want, v0) }); len(stale) > 0 {
		gone := db.Where("ptype = ? AND v1 = ? AND v0 = ANY(?)", ptypePolicy, SuperKey, store.Strings(stale)).Delete(&model.CasbinRule{})
		if gone.Error != nil {
			return 0, gone.Error
		}
		n = gone.RowsAffected
	}
	var rows []model.CasbinRule
	for _, v0 := range want {
		if !slices.Contains(have, v0) {
			rows = append(rows, model.CasbinRule{Ptype: ptypePolicy, V0: v0, V1: SuperKey, V2: AllRows})
		}
	}
	return n + int64(len(rows)), insert(db, rows)
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
// API accepts ([a-z][a-z0-9_]*) or a built-in's: nothing to quote. The
// core never runs it: it is text for the operator (SyncCore's error).
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

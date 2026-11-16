package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// SyncCore upserts the core permission catalog, marks core permissions no
// longer in code as removed, seeds built-in roles (failing when a custom role
// holds a built-in key, builtinKeyConflict) and grants newly created
// core permissions and built-in roles (as assignable) to the built-in roles
// whose defaults include them. A built-in role whose defaults_rev is behind
// its rev (an upgrade from a core without it, or defaults that changed) is
// brought to its defaults once (advanceDefaults): reset to them when nobody
// holds it, otherwise given every default it lacks, nothing it holds taken
// away. Either way what an administrator changes afterwards stays as
// changed. Safe to run concurrently from several nodes: each runs under the
// authz lock, so it reads what is there and writes only what differs. The
// version is only bumped when something changed.
func (s *Service) SyncCore(ctx context.Context) error {
	defs := CorePermissions()
	keys := make([]string, len(defs))
	for i, d := range defs {
		keys[i] = d.Key
	}
	var changed bool
	var v int64
	err := s.db.Tx(ctx, func(tx *gorm.DB) error {
		changed = false
		db := store.G(ctx, tx)
		if err := lockVersion(db); err != nil {
			return err
		}
		if err := builtinKeyConflict(db); err != nil {
			return err
		}
		// Built-in roles: the ones created or fixed now.
		newRoles := map[string]bool{}
		if err := syncRoles(db, func(k string, created bool) {
			newRoles[k] = created
			changed = true
		}); err != nil {
			return err
		}
		// The superuser role's grant of every key follows the flag the
		// roles were just given (an unchanged database changes nothing).
		supers, err := casbinrule.SyncSuperusers(db)
		if err != nil {
			return err
		}
		changed = changed || supers > 0
		// The roles getting every default they lack, with what the audit
		// says of why: created now, or behind their rev and held; and
		// those behind their rev that nobody holds, reset to them.
		fills := map[string]map[string]any{}
		for k, created := range newRoles {
			if created {
				fills[k] = map[string]any{"role_created": true}
			}
		}
		resets := map[string]bool{}
		if err := s.advanceDefaults(ctx, tx, func(k string, rev int32, held bool) {
			if held {
				fills[k] = map[string]any{"defaults_rev": rev}
			} else {
				resets[k] = true
			}
			changed = true
		}); err != nil {
			return err
		}

		// Permissions: the ones inserted now.
		inserted, modified, err := syncPermissions(db, defs)
		if err != nil {
			return err
		}
		changed = changed || modified

		// Built-in roles get their default permissions and assignable roles:
		// a reset writes them all (the permissions created above included),
		// the others get theirs through grantDefaults.
		for _, r := range builtinRoles {
			if resets[r.key] {
				if err := s.resetUnheld(ctx, tx, r); err != nil {
					return err
				}
				continue
			}
			fill := fills[r.key]
			// Scoped defaults are granted on a fill only: the actions exist
			// already, so none is new.
			if fill != nil && len(r.scoped) > 0 {
				if err := s.grantScopedDefaults(ctx, tx, r.key, r.scoped, fill); err != nil {
					return err
				}
			}
			if err := s.grantDefaults(ctx, tx, r.key, fill, r.unscopedDefaults(),
				func(k string) bool { return slices.Contains(inserted, k) }, "default", grantDefaultPermissions); err != nil {
				return err
			}
			if err := s.grantDefaults(ctx, tx, r.key, fill, r.assignable,
				func(k string) bool { return newRoles[k] }, "assignable", grantDefaultAssignable); err != nil {
				return err
			}
		}

		removed := removeUndeclaredCore(db, keys)
		if removed.Error != nil {
			return removed.Error
		}
		changed = changed || removed.RowsAffected > 0
		if changed {
			if v, err = s.bumpPolicy(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		s.Committed(ctx, v)
	}
	return nil
}

// lockVersion locks the authz_meta row without bumping it: SyncCore and
// ClearScopeGrants serialize with every authz write and bump only when they
// changed something.
func lockVersion(db *gorm.DB) error {
	if err := store.RequireTx(db); err != nil {
		return err
	}
	var m model.AuthzMeta
	return db.Select("version").Where("id = 1").Clauses(store.ForUpdate()).Find(&m).Error
}

// syncRoles creates the built-in roles (builtinRoles) missing and fixes the
// superuser flag of existing ones (their revision moves), calling synced
// with the key of each role created or fixed and whether it was created.
func syncRoles(db *gorm.DB, synced func(key string, created bool)) error {
	keys := make(store.Strings, len(builtinRoles))
	for i, r := range builtinRoles {
		keys[i] = r.key
	}
	var have []model.Role
	if err := db.Select("id", "key", "superuser").Where("key = ANY(?)", keys).Find(&have).Error; err != nil {
		return err
	}
	var rows []model.Role
	for _, r := range builtinRoles {
		i := slices.IndexFunc(have, func(h model.Role) bool { return h.Key == r.key })
		if i >= 0 {
			if have[i].Superuser != r.superuser {
				fix := map[string]any{"superuser": r.superuser, "revision": gorm.Expr("revision + 1")}
				if err := db.Model(&model.Role{}).Where("id = ?", have[i].ID).Updates(fix).Error; err != nil {
					return err
				}
				synced(r.key, false)
			}
			continue
		}
		name, err := json.Marshal(r.name)
		if err != nil {
			return err
		}
		desc, err := json.Marshal(r.description)
		if err != nil {
			return err
		}
		rows = append(rows, model.Role{Key: r.key, Name: name, Description: desc, Builtin: true, Superuser: r.superuser,
			AssignCustom: r.assignCustom, DefaultsRev: int(r.rev)})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := db.Create(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		synced(r.Key, true)
	}
	return nil
}

// syncPermissions writes the core permissions defs as active, inserting
// the missing ones and updating those whose stored columns differ, and
// returns the keys it inserted and whether it wrote any row.
func syncPermissions(db *gorm.DB, defs []core.PermissionDef) (inserted []string, changed bool, err error) {
	keys := make(store.Strings, len(defs))
	for i, d := range defs {
		keys[i] = d.Key
	}
	var have []model.Permission
	if err := db.Where("key = ANY(?)", keys).Find(&have).Error; err != nil {
		return nil, false, err
	}
	var rows []model.Permission
	for _, d := range defs {
		want, err := corePermissionRow(d)
		if err != nil {
			return nil, false, err
		}
		i := slices.IndexFunc(have, func(p model.Permission) bool { return p.Key == d.Key })
		if i < 0 {
			rows = append(rows, want)
			continue
		}
		if samePermission(have[i], want) {
			continue
		}
		changed = true
		if err := db.Model(&model.Permission{}).Where("id = ?", have[i].ID).Updates(map[string]any{
			"module": want.Module, "label": want.Label, "description": want.Description, "risk": want.Risk,
			"requires": want.Requires, "status": want.Status, "sort": want.Sort, "scope_type": want.ScopeType,
		}).Error; err != nil {
			return nil, false, err
		}
	}
	if len(rows) == 0 {
		return nil, changed, nil
	}
	if err := db.Create(&rows).Error; err != nil {
		return nil, false, err
	}
	for _, r := range rows {
		inserted = append(inserted, r.Key)
	}
	return inserted, true, nil
}

// corePermissionRow is the row of the core permission d, active.
func corePermissionRow(d core.PermissionDef) (model.Permission, error) {
	label, err := json.Marshal(d.Label)
	if err != nil {
		return model.Permission{}, err
	}
	desc, err := json.Marshal(d.Description)
	if err != nil {
		return model.Permission{}, err
	}
	risk, status := d.Risk, "active"
	return model.Permission{Key: d.Key, Module: d.Module, Label: label, Description: desc, Source: "core", Risk: &risk,
		Requires: append(store.Strings{}, d.Requires...), Status: &status, Sort: d.Sort, ScopeType: d.ScopeType}, nil
}

// samePermission reports whether the stored core permission have holds
// what want writes (sensitive follows risk): JSON compared by value.
func samePermission(have, want model.Permission) bool {
	return have.Module == want.Module && sameJSON(have.Label, want.Label) && sameJSON(have.Description, want.Description) &&
		deref(have.Risk) == deref(want.Risk) && have.Risk != nil && slices.Equal(have.Requires, want.Requires) &&
		deref(have.Status) == deref(want.Status) && have.Status != nil && have.Sort == want.Sort && have.ScopeType == want.ScopeType
}

// sameJSON reports whether a and b are the same JSON value (jsonb
// equality: key order and spacing aside).
func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(va, vb)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// grantDefaultPermissions grants the role roleKey the permissions of keys
// (for every row) it lacks, returning what it granted.
func grantDefaultPermissions(db *gorm.DB, roleKey string, keys []string) ([]casbinrule.Added, error) {
	grants := make([]casbinrule.Grant, len(keys))
	for i, k := range keys {
		grants[i] = casbinrule.Grant{Role: roleKey, Action: k}
	}
	return casbinrule.AddGrants(db, grants, casbinrule.Filter{})
}

// grantDefaultAssignable adds to the assignable list of the role roleKey
// the built-in, non-superuser roles of keys it lacks, returning them by
// role id and key; the role's revision moves when it added any.
func grantDefaultAssignable(db *gorm.DB, roleKey string, keys []string) ([]casbinrule.Added, error) {
	var found []struct {
		RoleID, AssignableRoleID int64
		Key                      string
		Held                     bool
	}
	err := db.Table("roles a").Select("r.id AS role_id, a.id AS assignable_role_id, a.key, ra.role_id IS NOT NULL AS held").
		Joins("JOIN roles r ON r.key = ?", roleKey).
		Joins("LEFT JOIN role_assignable_roles ra ON ra.role_id = r.id AND ra.assignable_role_id = a.id").
		Where("a.key = ANY(?) AND a.builtin AND NOT a.superuser", store.Strings(keys)).Order("a.key").Scan(&found).Error
	if err != nil {
		return nil, err
	}
	var rows []model.RoleAssignableRole
	added := []casbinrule.Added{}
	for _, f := range found {
		if !f.Held {
			rows = append(rows, model.RoleAssignableRole{RoleID: f.RoleID, AssignableRoleID: f.AssignableRoleID})
			added = append(added, casbinrule.Added{RoleID: f.RoleID, Key: f.Key})
		}
	}
	if len(rows) == 0 {
		return added, nil
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return nil, err
	}
	return added, reviseRole(db, rows[0].RoleID).Error
}

// removeUndeclaredCore marks removed the core permissions but keys.
func removeUndeclaredCore(db *gorm.DB, keys []string) *gorm.DB {
	return db.Model(&model.Permission{}).Where("source = 'core' AND status <> 'removed' AND key <> ALL(?)", store.Strings(keys)).Update("status", "removed")
}

// builtinKeyConflict fails when a custom role holds one of the built-in role
// keys: the role is never turned into the built-in one, which would hand its
// holders that role's defaults, and never skipped, which would leave the
// built-in role missing. The error stops startup and says how to rename it.
func builtinKeyConflict(db *gorm.DB) error {
	keys := make(store.Strings, len(builtinRoles))
	for i, r := range builtinRoles {
		keys[i] = r.key
	}
	var roles []model.Role
	if err := db.Select("id", "key").Where("key = ANY(?) AND NOT builtin", keys).Order("id").Find(&roles).Error; err != nil || len(roles) == 0 {
		return err
	}
	msg := "authz: custom roles hold keys reserved for built-in roles; rename each one (any free key will do) and start again:"
	for _, r := range roles {
		msg += fmt.Sprintf("\n  role id %d key %q: %s", r.ID, r.Key, casbinrule.RenameRoleSQL(r.ID, r.Key, r.Key+"_custom"))
	}
	return errors.New(msg)
}

// advanceDefaults moves each built-in role whose defaults_rev is behind its
// rev up to it and calls behind with the role, the rev reached and whether
// it is held (by an undeleted user; the super admin role, never reset,
// counts as held): SyncCore then grants a held role the defaults it lacks,
// turned assign_custom on here where its defaults have it (audited; the
// role's revision moves), and resets one nobody holds (resetUnheld).
func (s *Service) advanceDefaults(ctx context.Context, tx *gorm.DB, behind func(key string, rev int32, held bool)) error {
	db := store.G(ctx, tx)
	keys := make(store.Strings, len(builtinRoles))
	for i, r := range builtinRoles {
		keys[i] = r.key
	}
	var roles []model.Role
	if err := db.Select("id", "key", "superuser", "assign_custom", "defaults_rev").Where("key = ANY(?) AND builtin", keys).Order("id").Find(&roles).Error; err != nil {
		return err
	}
	for _, r := range roles {
		def, _ := builtinRoleByKey(r.Key)
		if r.DefaultsRev >= int(def.rev) {
			continue
		}
		held := r.Superuser
		if !held {
			holders := db.Model(&model.User{}).Where("deleted_at IS NULL AND id IN (?)", casbinrule.MembersOf(db, db.Model(&model.Role{}).Where("id = ?", r.ID)))
			var err error
			if held, err = gormpg.Exists(db, holders); err != nil {
				return err
			}
		}
		custom := held && def.assignCustom && !r.AssignCustom
		set := map[string]any{"defaults_rev": def.rev}
		if custom {
			set["assign_custom"], set["revision"] = true, gorm.Expr("revision + 1")
		}
		if err := db.Model(&model.Role{}).Where("id = ?", r.ID).Updates(set).Error; err != nil {
			return err
		}
		behind(r.Key, def.rev, held)
		if !custom {
			continue
		}
		if err := s.audit(ctx, tx, 0, "authz.sync.grant", "role", r.ID, map[string]any{"via": "assignable", "assign_custom": true, "defaults_rev": def.rev}); err != nil {
			return err
		}
	}
	return nil
}

// resetUnheld resets the built-in role def, behind its rev with nobody
// holding it, to its defaults as ResetRole does (resetDefaults, by the
// system, audited with via sync and the rev): no user's rights change, and
// whoever is given the role next gets what its defaults say.
func (s *Service) resetUnheld(ctx context.Context, tx *gorm.DB, def builtinRole) error {
	r, err := readRole(roleByKey(store.G(ctx, tx), def.key))
	if err != nil {
		return err
	}
	if err := reviseRole(store.G(ctx, tx), r.ID).Error; err != nil {
		return err
	}
	return s.resetDefaults(ctx, tx, 0, r, def, map[string]any{"via": "sync", "defaults_rev": def.rev})
}

// grantDefaults grants the built-in role roleKey its defaults through grant
// (grantDefaultPermissions or grantDefaultAssignable). On a fill (a role created now, or behind its rev) it
// gets every one it lacks, otherwise only the defaults created now; every
// grant is audited with via and the fill's detail (role_created or
// defaults_rev), so a role the sync creates or fills in on an upgrade shows
// what it was given.
func (s *Service) grantDefaults(ctx context.Context, tx *gorm.DB, roleKey string, fill map[string]any, defaults []string, created func(string) bool, via string,
	grant func(db *gorm.DB, roleKey string, keys []string) ([]casbinrule.Added, error)) error {
	if fill == nil {
		defaults = slices.DeleteFunc(slices.Clone(defaults), func(k string) bool { return !created(k) })
	}
	if len(defaults) == 0 {
		return nil
	}
	added, err := grant(store.G(ctx, tx), roleKey, defaults)
	if err != nil {
		return err
	}
	return s.syncGrant(ctx, tx, withVia(fill, via), added)
}

// withVia is the audit detail of a sync grant: via plus the fill's detail.
func withVia(fill map[string]any, via string) map[string]any {
	detail := maps.Clone(fill)
	if detail == nil {
		detail = map[string]any{}
	}
	detail["via"] = via
	return detail
}

// grantScopedDefaults grants the built-in role roleKey the scoped defaults
// (canonical keys) it lacks, each audited as authz.sync.grant (via default
// and the fill's detail). One the role's unscoped grant of the same action
// implies is left out, as migration 0097 leaves out a redundant own scope.
func (s *Service) grantScopedDefaults(ctx context.Context, tx *gorm.DB, roleKey string, keys []string, fill map[string]any) error {
	grants := make([]casbinrule.Grant, 0, len(keys))
	for _, k := range keys {
		p, err := perm.Parse(k)
		if err != nil {
			return fmt.Errorf("authz: scoped default %q of role %s: %w", k, roleKey, err)
		}
		grants = append(grants, casbinrule.Grant{Role: roleKey, Action: string(p.Action), Suffix: p.Suffix()})
	}
	added, err := casbinrule.AddGrants(store.G(ctx, tx), grants, casbinrule.Filter{Source: "core"})
	if err != nil {
		return err
	}
	return s.syncGrant(ctx, tx, withVia(fill, "default"), added)
}

// syncGrant audits each automatic grant of added (by role id, of a
// permission or an assignable role key) as authz.sync.grant by the system
// with via plus the key as detail.
func (s *Service) syncGrant(ctx context.Context, tx *gorm.DB, via map[string]any, added []casbinrule.Added) error {
	for _, g := range added {
		detail := maps.Clone(via)
		detail["key"] = g.Key
		if err := s.audit(ctx, tx, 0, "authz.sync.grant", "role", g.RoleID, detail); err != nil {
			return err
		}
	}
	return nil
}

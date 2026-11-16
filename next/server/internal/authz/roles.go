package authz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Role is the API view of a role.
type Role struct {
	ID             int64              `json:"id"`
	Key            string             `json:"key"`
	Name           core.LocalizedText `json:"name"`
	Description    core.LocalizedText `json:"description"`
	Builtin        bool               `json:"builtin"`
	Superuser      bool               `json:"superuser"`
	PermissionKeys []string           `json:"permission_keys"`
	// AssignableRoleKeys lists the roles its holders may assign, and
	// AssignCustom adds every custom role (role_assignable_roles).
	AssignableRoleKeys []string  `json:"assignable_role_keys"`
	AssignCustom       bool      `json:"assign_custom"`
	UserCount          int64     `json:"user_count"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// Revision moves with every change to the role (roles.revision): its
	// writes send it back to be refused (version_conflict) when the role
	// changed since it was read.
	Revision int64 `json:"revision"`
}

var roleKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,49}$`)

var (
	errRoleNotFound        = core.ErrNotFound.WithMessage("role not found")
	errBuiltinRole         = core.ErrConflict.WithMessage("built-in roles cannot be deleted")
	errNotBuiltinRole      = core.ErrConflict.WithMessage("only built-in roles can be reset")
	errSuperuserRoleFrozen = core.ErrConflict.WithMessage("the super admin role cannot be changed")
	errLastSuperAdmin      = core.ErrConflict.WithMessage("the last super admin cannot be removed")
)

// errRoleKeyImmutable is why PATCH /roles/:id refuses a different key, in the
// request's language.
func errRoleKeyImmutable(ctx context.Context) error {
	msg := t(ctx, "a role's key cannot be changed after creation", "角色标识创建后不可修改")
	return core.InvalidFields(core.FieldError{Field: "key", Code: "immutable", Message: msg}).WithMessage(msg)
}

// roleColumns are what scanRole reads of the role r, as the console shows
// and edits it; a grant of a staged permission (core.PermissionDef.Staged)
// is no editor's to see.
const roleColumns = `r.id, r.key, r.name, r.description, r.builtin, r.superuser, r.created_at, r.updated_at,
    COALESCE((SELECT array_agg(rg.key ORDER BY p.sort, rg.key) FROM (` + casbinrule.GrantsOfRole + `) rg
        JOIN permissions p ON p.key = rg.action AND p.status <> 'staged'), '{}'),
    COALESCE((SELECT array_agg(ar.key ORDER BY ar.key) FROM role_assignable_roles ra
        JOIN roles ar ON ar.id = ra.assignable_role_id AND NOT ar.superuser WHERE ra.role_id = r.id), '{}'),
    r.assign_custom,
    ` + roleHolders + `,
    r.revision`

// roleQuery selects the roles r as readRoles reads them (roleColumns).
func roleQuery(db *gorm.DB) *gorm.DB {
	return db.Table("roles r").Select(roleColumns)
}

// roleHolders counts the holders of role r, soft-deleted users aside: the
// roles page's user count.
const roleHolders = `(SELECT count(*) FROM users u WHERE u.id IN (` + casbinrule.MembersOfRole + `) AND u.deleted_at IS NULL)`

// listedRoles selects every role, built-in first.
func listedRoles(db *gorm.DB) *gorm.DB {
	return roleQuery(db).Order("r.builtin DESC, r.id")
}

// roleByID selects role id.
func roleByID(db *gorm.DB, id int64) *gorm.DB {
	return roleQuery(db).Where("r.id = ?", id)
}

// roleByKey selects the role key.
func roleByKey(db *gorm.DB, key string) *gorm.DB {
	return roleQuery(db).Where("r.key = ?", key)
}

// rolesByKeys selects the roles of keys, by key.
func rolesByKeys(db *gorm.DB, keys []string) *gorm.DB {
	return roleQuery(db).Where("r.key = ANY(?)", store.Strings(keys)).Order("r.key")
}

// readRoles reads the roles q, a roleQuery, returns (never nil).
func readRoles(q *gorm.DB) ([]*Role, error) {
	return store.CollectRows(q, scanRole)
}

// readRole is readRoles of a query of one role, failing with sql.ErrNoRows
// (store.IsNoRows) when there is none.
func readRole(q *gorm.DB) (*Role, error) {
	roles, err := readRoles(q)
	switch {
	case err != nil:
		return nil, err
	case len(roles) == 0:
		return nil, sql.ErrNoRows
	}
	return roles[0], nil
}

func scanRole(rows *sql.Rows) (*Role, error) {
	var r Role
	var name, desc store.JSON[core.LocalizedText]
	var perms, assignable store.Strings
	err := rows.Scan(&r.ID, &r.Key, &name, &desc, &r.Builtin, &r.Superuser, &r.CreatedAt, &r.UpdatedAt, &perms, &assignable, &r.AssignCustom, &r.UserCount, &r.Revision)
	if err != nil {
		return nil, err
	}
	r.Name, r.Description, r.AssignableRoleKeys = name.V, desc.V, assignable
	r.PermissionKeys = canonicalKeys(perms)
	if r.Name == nil {
		r.Name = core.LocalizedText{}
	}
	if r.Description == nil {
		r.Description = core.LocalizedText{}
	}
	return &r, nil
}

// ListRoles returns all roles, built-in first.
func (s *Service) ListRoles(ctx context.Context) ([]*Role, error) {
	return listRoles(ctx, s.db.Gorm)
}

func listRoles(ctx context.Context, q *gorm.DB) ([]*Role, error) {
	return readRoles(listedRoles(store.G(ctx, q)))
}

// GetRole returns one role or ErrNotFound.
func (s *Service) GetRole(ctx context.Context, id int64) (*Role, error) {
	return s.getRole(ctx, s.db.Gorm, id)
}

func (s *Service) getRole(ctx context.Context, q *gorm.DB, id int64) (*Role, error) {
	r, err := readRole(roleByID(store.G(ctx, q), id))
	if store.IsNoRows(err) {
		return nil, errRoleNotFound
	}
	return r, err
}

// editableRole locks role id inside tx, which has called Bump, and returns
// it, its revision moved, when actorID may change it and rev, when set, is
// its revision (else version_conflict: the editor read the role before
// someone else changed it): the super admin role is frozen for everyone
// (name, description, permissions, assignable list, reset, delete); any
// other role must be within the actor's grantSet (cannotActOnRole, M5: an
// actor who could weaken a role above it could then act on that role's
// holders) and, when it is built-in or has holders, on the actor's
// assignable list: a change to the role is a change to the users holding
// it, now or later, so it takes the role the actor manages them by (§43.6;
// assign_custom never covers the actor's own roles). With the default lists
// an admin thus changes operator, supplier and user but not admin. The
// system (actor 0) passes. Every role write goes through it.
func (s *Service) editableRole(ctx context.Context, tx *gorm.DB, actorID, id int64, rev *int64) (*Role, error) {
	if err := store.RequireTx(tx); err != nil {
		return nil, err
	}
	r, err := readRole(lockRole(store.G(ctx, tx), id))
	switch {
	case store.IsNoRows(err):
		return nil, errRoleNotFound
	case err != nil:
		return nil, err
	case r.Superuser:
		return nil, errSuperuserRoleFrozen
	case rev != nil && *rev != r.Revision:
		return nil, errVersionConflict(ctx, r.Revision)
	}
	if actorID != 0 {
		if err := s.mayEdit(ctx, tx, actorID, r); err != nil {
			return nil, err
		}
	}
	if err := reviseRole(store.G(ctx, tx), id).Error; err != nil {
		return nil, err
	}
	r.Revision++
	return r, nil
}

// mayEdit fails unless actorID may change role r (editDenial).
func (s *Service) mayEdit(ctx context.Context, tx *gorm.DB, actorID int64, r *Role) error {
	actor, err := s.grantSet(ctx, tx, actorID)
	if err != nil {
		return err
	}
	cur, err := s.RolesPermissions(ctx, tx, []string{r.Key})
	if err != nil {
		return err
	}
	switch reason, missing := editDenial(actor, cur, r); reason {
	case core.GrantDenyNotEditable:
		return cannotActOnRole(ctx).WithDetails(missing.Details())
	case core.GrantDenyNotAssignable:
		return notAssignable(ctx, r.Key)
	}
	return nil
}

// reviseRole moves the revision of role id: every change to it does.
func reviseRole(db *gorm.DB, id int64) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Update("revision", gorm.Expr("revision + 1"))
}

// lockRole selects role id and locks its roles row.
func lockRole(db *gorm.DB, id int64) *gorm.DB {
	return roleQuery(db).Where("r.id = ?", id).Clauses(store.ForUpdate().Of("r"))
}

// editDenial is why actor (its grantSet) may not change role r, whose
// grants are cur, "" when it may, with what actor misses of cur when that
// is why: editableRole's rule, a superuser role being frozen for everyone.
func editDenial(actor, cur core.PermissionSet, r *Role) (string, core.Missing) {
	if r.Superuser {
		return core.GrantDenySuperuserRole, core.Missing{}
	}
	if m := actor.Missing(cur); !m.Empty() {
		return core.GrantDenyNotEditable, m
	}
	if (r.Builtin || r.UserCount > 0) && !actor.CanAssign(r.Key) {
		return core.GrantDenyNotAssignable, core.Missing{}
	}
	return "", core.Missing{}
}

// CreateRoleInput is the body of POST /roles.
type CreateRoleInput struct {
	Key            string             `json:"key"`
	Name           core.LocalizedText `json:"name"`
	Description    core.LocalizedText `json:"description"`
	PermissionKeys []string           `json:"permission_keys"`
	// AssignableRoleKeys and AssignCustom are the role's assignable list.
	AssignableRoleKeys []string `json:"assignable_role_keys"`
	AssignCustom       bool     `json:"assign_custom"`
}

// CreateRole creates a custom (non-superuser) role; its key may not be one
// reserved for a built-in role (reservedRoleKey). The actor must hold all
// permissions being granted and be able to assign every role the new role
// may assign, as for SetRolePermissions and SetRoleAssignable (CONTRACTS
// §43.6), checked inside the transaction after Bump.
func (s *Service) CreateRole(ctx context.Context, actorID int64, in CreateRoleInput) (*Role, error) {
	var fields []core.FieldError
	if !roleKeyRe.MatchString(in.Key) {
		fields = append(fields, core.FieldError{Field: "key", Code: "invalid", Message: "must match ^[a-z][a-z0-9_]{1,49}$"})
	}
	if reservedRoleKey(in.Key) {
		fields = append(fields, core.FieldError{Field: "key", Code: "reserved", Message: "this key is reserved for a built-in role"})
	}
	if in.Name.Get("en") == "" {
		fields = append(fields, core.FieldError{Field: "name", Code: "required", Message: "name is required"})
	}
	if len(fields) > 0 {
		return nil, core.InvalidFields(fields...)
	}
	if in.Description == nil {
		in.Description = core.LocalizedText{}
	}
	return s.mutateRole(ctx, func(tx *gorm.DB) (int64, error) {
		keys, grants, err := resolvePermissions(ctx, tx, in.PermissionKeys)
		if err != nil {
			return 0, err
		}
		assign, err := assignableRoles(ctx, tx, in.AssignableRoleKeys)
		if err != nil {
			return 0, err
		}
		grant, err := grantOf(ctx, tx, in.Key, keys, roleKeysOf(assign), in.AssignCustom)
		if err != nil {
			return 0, err
		}
		if err := s.covers(ctx, tx, actorID, grant, cannotGrant(ctx)); err != nil {
			return 0, err
		}
		if err := s.stepUpCritical(ctx, grants, keys); err != nil {
			return 0, err
		}
		if err := checkScopeRefs(ctx, tx, keys); err != nil {
			return 0, err
		}
		id, err := insertRole(store.G(ctx, tx), in)
		if store.IsUniqueViolation(err, "") {
			return 0, core.ErrConflict.WithMessage("role key already exists")
		}
		if err != nil {
			return 0, err
		}
		if err := writeGrants(ctx, tx, in.Key, grants, grantPart{}); err != nil {
			return 0, err
		}
		if err := setRoleAssignable(ctx, tx, id, assign, in.AssignCustom); err != nil {
			return 0, err
		}
		return id, s.audit(ctx, tx, actorID, "role.create", "role", id, map[string]any{"key": in.Key, "permission_keys": keys,
			"assignable_role_keys": roleKeysOf(assign), "assign_custom": in.AssignCustom})
	})
}

// insertRole inserts the custom role of in (its key, name and
// description) and returns its id.
func insertRole(db *gorm.DB, in CreateRoleInput) (int64, error) {
	name, err := json.Marshal(in.Name)
	if err != nil {
		return 0, err
	}
	desc, err := json.Marshal(in.Description)
	if err != nil {
		return 0, err
	}
	r := model.Role{Key: in.Key, Name: name, Description: desc}
	err = db.Clauses(clause.Returning{Columns: []clause.Column{{Name: "id"}}}).Create(&r).Error
	return r.ID, err
}

// UpdateRoleInput is the body of PATCH /roles/:id.
type UpdateRoleInput struct {
	// Revision, when sent, must be the role's (Role.Revision), else
	// version_conflict.
	Revision *int64 `json:"revision,omitempty"`
	// Key, when sent, must be the role's key: a key never changes
	// (errRoleKeyImmutable).
	Key         *string            `json:"key,omitempty"`
	Name        core.LocalizedText `json:"name"`
	Description core.LocalizedText `json:"description"`
}

// UpdateRole changes name/description of a role actorID may edit
// (editableRole; built-in roles included). A role's key never changes: a
// different key in the body is refused, the same one ignored.
func (s *Service) UpdateRole(ctx context.Context, actorID, id int64, in UpdateRoleInput) (*Role, error) {
	if in.Name != nil && in.Name.Get("en") == "" {
		return nil, core.InvalidFields(core.FieldError{Field: "name", Code: "required", Message: "name is required"})
	}
	return s.mutateRole(ctx, func(tx *gorm.DB) (int64, error) {
		r, err := s.editableRole(ctx, tx, actorID, id, in.Revision)
		if err != nil {
			return 0, err
		}
		if in.Key != nil && *in.Key != r.Key {
			return 0, errRoleKeyImmutable(ctx)
		}
		if err := updateRole(store.G(ctx, tx), id, in).Error; err != nil {
			return 0, err
		}
		return id, s.audit(ctx, tx, actorID, "role.update", "role", id, in)
	})
}

// updateRole writes the name and description of in to role id, each kept
// when not sent, and touches updated_at.
func updateRole(db *gorm.DB, id int64, in UpdateRoleInput) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Updates(map[string]any{
		"name":        gorm.Expr("COALESCE(?, name)", nilIfEmpty(in.Name)),
		"description": gorm.Expr("COALESCE(?, description)", nilIfEmpty(in.Description)),
		"updated_at":  gorm.Expr("now()"),
	})
}

func nilIfEmpty(t core.LocalizedText) any {
	if t == nil {
		return nil
	}
	return t
}

// DeleteRole deletes a custom role actorID may edit (editableRole: while it
// has holders, deleting it revokes it, and the role must be on the actor's
// assignable list) with its grants and assignments.
func (s *Service) DeleteRole(ctx context.Context, actorID, id int64) error {
	return s.mutate(ctx, func(tx *gorm.DB) error {
		r, err := s.editableRole(ctx, tx, actorID, id, nil)
		if err != nil {
			return err
		}
		if r.Builtin {
			return errBuiltinRole
		}
		if err := casbinrule.DeleteRole(store.G(ctx, tx), r.Key); err != nil {
			return err
		}
		if err := reviseListing(store.G(ctx, tx), id).Error; err != nil {
			return err
		}
		if err := deleteRole(store.G(ctx, tx), id).Error; err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "role.delete", "role", id, map[string]any{"key": r.Key})
	})
}

// reviseListing moves the revision of the roles whose assignable list
// names role id: deleting it takes it off those lists (the cascade of
// role_assignable_roles), a change to each of those roles.
func reviseListing(db *gorm.DB, id int64) *gorm.DB {
	return db.Model(&model.Role{}).Where("id IN (?)", db.Model(&model.RoleAssignableRole{}).Select("role_id").Where("assignable_role_id = ?", id)).
		Update("revision", gorm.Expr("revision + 1"))
}

// deleteRole deletes role id; its assignable lists cascade (its grants and
// assignments are casbin_rule's, deleted first).
func deleteRole(db *gorm.DB, id int64) *gorm.DB {
	return db.Delete(&model.Role{}, id)
}

// ResetRole restores a built-in role actorID may edit (editableRole) to its
// defaults (builtinRoles): name, description and what resetDefaults
// writes. The actor must hold every default, as for SetRolePermissions and
// SetRoleAssignable; what the reset drops is within the actor already
// (editableRole), so it cannot weaken a role above the actor (C-L2). rev,
// when set, must be the role's revision, else version_conflict.
func (s *Service) ResetRole(ctx context.Context, actorID, id int64, rev *int64) (*Role, error) {
	return s.mutateRole(ctx, func(tx *gorm.DB) (int64, error) {
		r, err := s.editableRole(ctx, tx, actorID, id, rev)
		if err != nil {
			return 0, err
		}
		def, ok := builtinRoleByKey(r.Key)
		if !r.Builtin || !ok {
			return 0, errNotBuiltinRole
		}
		if err := resetRoleText(store.G(ctx, tx), id, def).Error; err != nil {
			return 0, err
		}
		return id, s.resetDefaults(ctx, tx, actorID, r, def, map[string]any{})
	})
}

// resetRoleText writes the name and description of def to role id;
// updated_at stays, as it always has for a reset.
func resetRoleText(db *gorm.DB, id int64, def builtinRole) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Updates(map[string]any{"name": def.name, "description": def.description})
}

// resetDefaults makes def, the defaults of built-in role r, its core
// permissions (scoped ones included), assignable list and assign_custom,
// marks them applied (defaults_rev) and audits it as role.reset by actorID
// with detail, to which it adds the key and the role before and after
// (roleDetail). actorID must hold all the defaults hand out (covers; the
// system passes). Plugin permissions on the role are left alone: they are
// granted per plugin at consent time. Defaults naming a built-in role not
// created yet are skipped, as SyncCore does. ResetRole and SyncCore (a
// role nobody holds, behind its rev) both reset through it.
func (s *Service) resetDefaults(ctx context.Context, tx *gorm.DB, actorID int64, r *Role, def builtinRole, detail map[string]any) error {
	keys, grants, err := resolvePermissions(ctx, tx, def.defaultPermissions())
	if err != nil {
		return err
	}
	assign, err := queryRoles(assignableBuiltinRefs(store.G(ctx, tx), def.assignable))
	if err != nil {
		return err
	}
	grant, err := grantOf(ctx, tx, r.Key, keys, roleKeysOf(assign), def.assignCustom)
	if err != nil {
		return err
	}
	if err := s.covers(ctx, tx, actorID, grant, cannotGrant(ctx)); err != nil {
		return err
	}
	if err := setRoleAssignable(ctx, tx, r.ID, assign, def.assignCustom); err != nil {
		return err
	}
	if err := setDefaultsRev(store.G(ctx, tx), r.ID, def.rev).Error; err != nil {
		return err
	}
	if err := writeGrants(ctx, tx, r.Key, grants, grantPart{coreOnly: true}); err != nil {
		return err
	}
	after, err := s.getRole(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	detail["key"], detail["before"], detail["after"] = r.Key, roleDetail(r), roleDetail(after)
	return s.audit(ctx, tx, actorID, "role.reset", "role", r.ID, detail)
}

// setDefaultsRev marks the defaults of rev applied to role id; updated_at
// stays.
func setDefaultsRev(db *gorm.DB, id int64, rev int32) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Update("defaults_rev", rev)
}

// roleDetail is what a role grants, for an audit's before and after.
func roleDetail(r *Role) map[string]any {
	return map[string]any{"permission_keys": r.PermissionKeys, "assignable_role_keys": r.AssignableRoleKeys, "assign_custom": r.AssignCustom}
}

// SetRolePermissions replaces the permissions of a role actorID may edit
// (editableRole), as setRolePart does. rev, when set, must be the role's
// revision the editor read (Role.Revision), else version_conflict.
func (s *Service) SetRolePermissions(ctx context.Context, actorID, id int64, keys []string, rev *int64) (*Role, error) {
	return s.mutateRole(ctx, func(tx *gorm.DB) (int64, error) {
		r, err := s.editableRole(ctx, tx, actorID, id, rev)
		if err != nil {
			return 0, err
		}
		return id, s.setRolePart(ctx, tx, actorID, r, keys, grantPart{}, map[string]any{})
	})
}

// setRolePart makes keys the grants of r (locked by editableRole) within
// part. actorID must hold what keys add beyond what r grants (CONTRACTS
// §4.1, SEC-H2); what r keeps is within it already. Adding a critical
// plugin permission takes a step-up proof (stepUpCritical). A removed
// permission r holds stays when listed, so a role saves back as listed
// (P2), and goes when not; one it does not hold is ignored (writeGrants).
// Audited as role.permissions with detail, to which it adds the keys, what
// was added and what was dropped (removed permissions included); a write of
// one plugin's part adds that part before and after, its permission_keys
// being that part only.
func (s *Service) setRolePart(ctx context.Context, tx *gorm.DB, actorID int64, r *Role, keys []string, part grantPart, detail map[string]any) error {
	live, grants, err := resolvePermissions(ctx, tx, keys)
	if err != nil {
		return err
	}
	added := notImplied(live, r.PermissionKeys)
	if err := s.covers(ctx, tx, actorID, core.PermissionSetOf(added...), cannotGrant(ctx)); err != nil {
		return err
	}
	if err := s.stepUpCritical(ctx, grants, added); err != nil {
		return err
	}
	if err := checkScopeRefs(ctx, tx, added); err != nil {
		return err
	}
	if err := writeGrants(ctx, tx, r.Key, grants, part); err != nil {
		return err
	}
	if err := touchRole(store.G(ctx, tx), r.ID).Error; err != nil {
		return err
	}
	// What r holds within part now (writeGrants): the live keys and the
	// removed ones it held and kept.
	held := slices.Clone(live)
	for _, g := range grants {
		if !slices.Contains(live, g.key) && slices.Contains(r.PermissionKeys, g.key) {
			held = append(held, g.key)
		}
	}
	before, now := part.of(r.PermissionKeys), part.of(held)
	detail["permission_keys"], detail["added"], detail["removed"] = live, added, keysNotIn(before, now)
	if part.plugin != "" {
		detail["before"], detail["after"] = before, now
	}
	return s.audit(ctx, tx, actorID, "role.permissions", "role", r.ID, detail)
}

// touchRole sets the updated_at of role id to now.
func touchRole(db *gorm.DB, id int64) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Update("updated_at", gorm.Expr("now()"))
}

// stepUpCritical asks the request for a step-up proof (plugin.grant) when
// added, the keys a write hands a role beyond what it had, holds a critical
// plugin permission of grants (CONTRACTS C3): the proof a consent or a
// review confirming the same grant takes.
func (s *Service) stepUpCritical(ctx context.Context, grants []grant, added []string) error {
	if !slices.ContainsFunc(grants, func(g grant) bool { return g.critical && slices.Contains(added, g.key) }) {
		return nil
	}
	return s.stepUpOrDeny().ConsumeStepUp(ctx, core.StepUpPluginGrant)
}

// AssignableInput is the body of PUT /roles/:id/assignable.
type AssignableInput struct {
	RoleKeys     []string `json:"role_keys"`
	AssignCustom bool     `json:"assign_custom"`
	// Revision, when sent, must be the role's, else version_conflict.
	Revision *int64 `json:"revision,omitempty"`
}

// SetRoleAssignable replaces the assignable list of a role actorID may edit
// (editableRole). The actor must be able to assign what is added (the
// "every custom role" flag included); what the role keeps is within it
// already.
func (s *Service) SetRoleAssignable(ctx context.Context, actorID, id int64, in AssignableInput) (*Role, error) {
	return s.mutateRole(ctx, func(tx *gorm.DB) (int64, error) {
		r, err := s.editableRole(ctx, tx, actorID, id, in.Revision)
		if err != nil {
			return 0, err
		}
		assign, err := assignableRoles(ctx, tx, in.RoleKeys)
		if err != nil {
			return 0, err
		}
		keys := roleKeysOf(assign)
		added := keysNotIn(keys, r.AssignableRoleKeys)
		grant, err := grantOf(ctx, tx, r.Key, nil, added, in.AssignCustom && !r.AssignCustom)
		if err != nil {
			return 0, err
		}
		if err := s.covers(ctx, tx, actorID, grant, cannotGrant(ctx)); err != nil {
			return 0, err
		}
		if err := setRoleAssignable(ctx, tx, id, assign, in.AssignCustom); err != nil {
			return 0, err
		}
		return id, s.audit(ctx, tx, actorID, "role.assignable", "role", id, map[string]any{"role_keys": keys, "assign_custom": in.AssignCustom, "added": added})
	})
}

// assignableRoles resolves keys for an assignable list: each must exist and
// none may be a superuser role, which is only ever transferred.
func assignableRoles(ctx context.Context, q *gorm.DB, keys []string) ([]roleRef, error) {
	roles, err := resolveRoles(ctx, q, dedupe(keys))
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(roles, func(r roleRef) bool { return r.superuser }) {
		return nil, core.InvalidFields(core.FieldError{Field: "role_keys", Code: "superuser", Message: "the super admin role cannot be assignable"})
	}
	return roles, nil
}

// grantOf is what a write to role roleKey hands out to its holders: perms,
// and roles plus, when custom, the "every custom role" flag and every custom
// role but roleKey as what they may assign, expanded as RolesPermissions
// does. Expanding it means the actor must be able to assign each of those
// roles: a wildcard never covers the actor's own role (§43.6), so a holder
// of a custom role with assign_custom cannot hand out, through another role,
// a wildcard that manages its own role's holders.
func grantOf(ctx context.Context, q *gorm.DB, roleKey string, perms, roles []string, custom bool) (core.PermissionSet, error) {
	set := core.PermissionSetOf(perms...)
	set.Assignable = core.PermissionSetOf(roles...).Keys
	set.AssignCustom = custom
	if !custom {
		return set, nil
	}
	keys, err := customRoleKeys(store.G(ctx, q), roleKey)
	if err != nil {
		return core.PermissionSet{}, err
	}
	for _, k := range keys {
		set.Assignable[k] = struct{}{}
	}
	return set, nil
}

// setRoleAssignable makes roles, plus every custom role when custom, the
// role's assignable list.
func setRoleAssignable(ctx context.Context, tx *gorm.DB, roleID int64, roles []roleRef, custom bool) error {
	ids := roleIDsOf(roles)
	db := store.G(ctx, tx)
	if err := dropAssignable(db, roleID, ids).Error; err != nil {
		return err
	}
	if err := addAssignable(db, roleID, ids); err != nil {
		return err
	}
	return setAssignCustom(db, roleID, custom).Error
}

// dropAssignable deletes the roles but ids from role roleID's assignable
// list.
func dropAssignable(db *gorm.DB, roleID int64, ids store.Int64s) *gorm.DB {
	return db.Where("role_id = ? AND assignable_role_id <> ALL(?)", roleID, ids).Delete(&model.RoleAssignableRole{})
}

// addAssignable adds the roles ids to role roleID's assignable list.
func addAssignable(db *gorm.DB, roleID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows := make([]model.RoleAssignableRole, len(ids))
	for i, id := range ids {
		rows[i] = model.RoleAssignableRole{RoleID: roleID, AssignableRoleID: id}
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

// setAssignCustom sets the assign_custom flag of role id and touches its
// updated_at.
func setAssignCustom(db *gorm.DB, id int64, custom bool) *gorm.DB {
	return db.Model(&model.Role{}).Where("id = ?", id).Updates(map[string]any{"assign_custom": custom, "updated_at": gorm.Expr("now()")})
}

// ------------------------------------------------------------ user roles (used by iam)

// SetUserRoles replaces a user's roles inside tx, bumping the authz version
// (call Committed with the returned version after commit). actorID is the
// acting user (0 = system); checkAssignment judges what the change adds and
// removes, and a change is audited as user.roles.
func (s *Service) SetUserRoles(ctx context.Context, tx *gorm.DB, actorID, userID int64, roleKeys []string) (int64, error) {
	v, err := s.Bump(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	want, err := resolveRoles(ctx, tx, dedupe(roleKeys))
	if err != nil {
		return 0, err
	}
	cur, err := queryRoles(userRoleRefs(store.G(ctx, tx), userID))
	if err != nil {
		return 0, err
	}
	added, removed := diffRoles(want, cur), diffRoles(cur, want)
	if len(added)+len(removed) == 0 {
		return v, nil
	}
	if err := s.checkAssignment(ctx, tx, actorID, userID, len(cur) == 0, want, added, removed); err != nil {
		return 0, err
	}
	if err := writeUserRoles(ctx, tx, userID, want); err != nil {
		return 0, err
	}
	return v, s.audit(ctx, tx, actorID, "user.roles", "user", userID, map[string]any{"added": roleKeysOf(added), "removed": roleKeysOf(removed)})
}

// checkAssignment fails unless actorID may add and remove these roles on
// userID: the super admin role is never assigned or revoked here (it is
// transferred); every role must be on the actor's assignable list, except
// the default role for a user who has none yet (roleless); the actor must
// be able to act on the user; and the added roles, with what they may assign
// in turn, must be within the actor's grantSet; a user with an owner keeps
// one that manages it by its roles, want (owner_cannot_manage, CONTRACTS
// §102.3). The system (actor 0) may grant the initial super admin, never
// revoke one.
func (s *Service) checkAssignment(ctx context.Context, q *gorm.DB, actorID, userID int64, roleless bool, want, added, removed []roleRef) error {
	if actorID == 0 {
		if slices.ContainsFunc(removed, func(r roleRef) bool { return r.superuser }) {
			return superAdminTransferOnly(ctx)
		}
		return nil
	}
	actor, err := s.grantSet(ctx, q, actorID)
	if err != nil {
		return err
	}
	for _, r := range slices.Concat(added, removed) {
		if err := assignable(ctx, actor, r, roleless); err != nil {
			return err
		}
	}
	if err := s.canActOnUsers(ctx, q, actorID, core.ActOnIdentity, userID); err != nil {
		return err
	}
	grant, err := s.RolesPermissions(ctx, q, roleKeysOf(added))
	if err != nil {
		return err
	}
	if err := within(actor, grant, cannotGrant(ctx)); err != nil || len(added) == 0 {
		return err
	}
	owner, err := ownerOf(store.G(ctx, q), userID)
	if err != nil || owner == 0 {
		return err
	}
	target, err := s.RolesPermissions(ctx, q, roleKeysOf(want))
	if err != nil {
		return err
	}
	return s.ownerManages(ctx, q, owner, target, roleKeysOf(want))
}

// assignable fails unless actor may assign or revoke r (see checkAssignment).
func assignable(ctx context.Context, actor core.PermissionSet, r roleRef, roleless bool) error {
	switch {
	case r.superuser:
		return superAdminTransferOnly(ctx)
	case actor.CanAssign(r.key), roleless && r.key == RoleUser:
		return nil
	}
	return notAssignable(ctx, r.key)
}

// roleRef is a role as a user's assignment holds it.
type roleRef struct {
	id        int64
	key       string
	superuser bool
}

// roleRefs selects the roles as queryRoles reads them, by key.
func roleRefs(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Role{}).Select("id", "key", "superuser").Order("key")
}

// userRoleRefs selects the roles userID holds, by key.
func userRoleRefs(db *gorm.DB, userID int64) *gorm.DB {
	return roleRefs(db).Where("key IN (?)", casbinrule.RoleKeysOf(db, []int64{userID}))
}

// roleRefsByKeys selects the roles of keys, by key.
func roleRefsByKeys(db *gorm.DB, keys []string) *gorm.DB {
	return roleRefs(db).Where("key = ANY(?)", store.Strings(keys))
}

// queryRoles reads the roles q, a roleRefs query (id, key, superuser),
// returns (never nil).
func queryRoles(q *gorm.DB) ([]roleRef, error) {
	return store.CollectRows(q, func(rows *sql.Rows) (roleRef, error) {
		var r roleRef
		err := rows.Scan(&r.id, &r.key, &r.superuser)
		return r, err
	})
}

// assignableBuiltinRefs selects the built-in roles of keys but superuser
// ones, by key: what a reset makes assignable.
func assignableBuiltinRefs(db *gorm.DB, keys []string) *gorm.DB {
	return roleRefsByKeys(db, keys).Where("builtin AND NOT superuser")
}

// resolveRoles returns the roles of keys, failing on unknown ones.
func resolveRoles(ctx context.Context, q *gorm.DB, keys []string) ([]roleRef, error) {
	roles, err := queryRoles(roleRefsByKeys(store.G(ctx, q), keys))
	if err != nil {
		return nil, err
	}
	if unknown := keysNotIn(keys, roleKeysOf(roles)); len(unknown) > 0 {
		return nil, core.InvalidFields(core.FieldError{Field: "role_keys", Code: "unknown", Message: "unknown roles: " + strings.Join(unknown, ", ")})
	}
	return roles, nil
}

// diffRoles returns the roles in a that are not in b.
func diffRoles(a, b []roleRef) []roleRef {
	return slices.DeleteFunc(slices.Clone(a), func(r roleRef) bool {
		return slices.ContainsFunc(b, func(o roleRef) bool { return o.id == r.id })
	})
}

func roleKeysOf(roles []roleRef) []string {
	keys := make([]string, len(roles))
	for i, r := range roles {
		keys[i] = r.key
	}
	return keys
}

func roleIDsOf(roles []roleRef) []int64 {
	ids := make([]int64, len(roles))
	for i, r := range roles {
		ids[i] = r.id
	}
	return ids
}

// writeUserRoles makes roles the user's roles.
func writeUserRoles(ctx context.Context, tx *gorm.DB, userID int64, roles []roleRef) error {
	return casbinrule.SetRoles(store.G(ctx, tx), userID, roleKeysOf(roles))
}

// EnsureNotLastSuperAdmin fails with conflict when userID is a superuser and
// no other active superuser exists. iam calls it inside the transaction that
// disables or deletes a user, after Bump (which serializes the check).
func (s *Service) EnsureNotLastSuperAdmin(ctx context.Context, tx *gorm.DB, userID int64) error {
	su, err := s.IsSuperuser(ctx, tx, userID)
	if err != nil || !su {
		return err
	}
	return s.ensureOtherSuperuser(ctx, tx, userID)
}

func (s *Service) ensureOtherSuperuser(ctx context.Context, tx *gorm.DB, userID int64) error {
	var n int64
	if err := otherSuperusers(store.G(ctx, tx), userID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return errLastSuperAdmin
	}
	return nil
}

// otherSuperusers selects the active, undeleted users but userID holding a
// superuser role.
func otherSuperusers(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.User{}).Where("id IN (?)", casbinrule.MembersOf(db, db.Model(&model.Role{}).Where("superuser"))).
		Where("deleted_at IS NULL AND status = 'active' AND id <> ?", userID)
}

// AssignableRole is one role in GET /me/assignable-roles.
type AssignableRole struct {
	Key     string             `json:"key"`
	Name    core.LocalizedText `json:"name"`
	Builtin bool               `json:"builtin"`
	// Grantable: what the role carries is within the caller, so assigning
	// it passes; the Missing fields say what is not, as in a denial.
	Grantable           bool     `json:"grantable"`
	Missing             []string `json:"missing"`
	MissingAssignable   []string `json:"missing_assignable"`
	MissingAssignCustom bool     `json:"missing_assign_custom"`
}

// AssignableRoles lists the roles on actorID's assignable list (its
// grantSet, so none under an access key), built-in first, never a superuser
// role, each with whether the actor may grant it.
func (s *Service) AssignableRoles(ctx context.Context, actorID int64) ([]AssignableRole, error) {
	q := s.db.Gorm
	actor, err := s.grantSet(ctx, q, actorID)
	if err != nil {
		return nil, err
	}
	var rows []assignableRoleRow
	if err := assignableRoleRows(store.G(ctx, q)).Scan(&rows).Error; err != nil {
		return nil, err
	}
	rows = slices.DeleteFunc(rows, func(r assignableRoleRow) bool { return !actor.CanAssign(r.Key) })
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.Key
	}
	// What each role carries, in one reading (as RolesPermissions of it
	// alone: the wildcard leaves only the role itself out).
	sets, err := s.eachRolePermissions(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	out := []AssignableRole{}
	for _, row := range rows {
		r := AssignableRole{Key: row.Key, Name: row.Name.V, Builtin: row.Builtin}
		grant, ok := sets[r.Key]
		if !ok {
			continue // deleted meanwhile
		}
		m := actor.Missing(grant)
		r.Grantable = m.Empty()
		r.Missing = append([]string{}, m.Keys...)
		r.MissingAssignable = append([]string{}, m.Assign...)
		r.MissingAssignCustom = m.AssignCustom
		out = append(out, r)
	}
	return out, nil
}

// assignableRoleRow is a role as AssignableRoles reads it.
type assignableRoleRow struct {
	Key     string                         `gorm:"column:key"`
	Name    store.JSON[core.LocalizedText] `gorm:"column:name"`
	Builtin bool                           `gorm:"column:builtin"`
}

// assignableRoleRows selects every role but superuser ones, built-in
// first.
func assignableRoleRows(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Role{}).Select("key", "name", "builtin").Where("NOT superuser").Order("builtin DESC, id")
}

// ------------------------------------------------------------ permission listing

// PermissionItem is one permission in GET /permissions.
type PermissionItem struct {
	Key         string             `json:"key"`
	Label       core.LocalizedText `json:"label"`
	Description core.LocalizedText `json:"description"`
	Risk        string             `json:"risk"` // manifest.RiskLow | RiskSensitive | RiskCritical
	// StepUp: using it may need a step-up proof (catalog stepUp).
	StepUp bool `json:"step_up"`
	// Requires: soft dependencies, keys it is of little use without (catalog
	// requires, manifest requires); editor hints only, never part of
	// authorization.
	Requires []string `json:"requires,omitempty"`
	// ScopeType: the key takes a scope over this resource ("account",
	// "proxy"; CONTRACTS §21; "plugin:<resource>"), "" for none.
	ScopeType string `json:"scope_type"`
	// Scope: a plugin permission's scope as declared, its resource the
	// ResourceName() ("value" for an enum); plugin scoped keys only.
	Scope *manifest.PermissionScope `json:"scope,omitempty"`
	Group string                    `json:"group,omitempty"` // a plugin permission's group key
	// Defaults: the plugin's suggestion as full canonical keys per built-in
	// role, comparable with Role.permission_keys as is (the console restores
	// it through the usual writes).
	Defaults map[string][]string `json:"defaults,omitempty"`
	Status   string              `json:"status"`
}

// PermissionModule groups permissions by module.
type PermissionModule struct {
	Module    string             `json:"module"`
	Label     core.LocalizedText `json:"label"`
	Source    string             `json:"source"`
	PluginKey *string            `json:"plugin_key"`
	Status    string             `json:"status"`
	// Groups: a plugin module's permission groups, by their sort.
	Groups      []PermissionGroup `json:"groups,omitempty"`
	Permissions []PermissionItem  `json:"permissions"`
}

// PermissionGroup is a plugin's permission group (manifest permissionGroups[]).
type PermissionGroup struct {
	Key   string             `json:"key"`
	Label core.LocalizedText `json:"label"`
	Sort  int                `json:"sort"`
}

// ListPermissions returns the catalog grouped by module: core modules in
// catalog order, then plugin modules by plugin key, each plugin's by group.
// Staged permissions, of a version not activated yet, are left out: no
// editor may grant them (the review lists them).
func (s *Service) ListPermissions(ctx context.Context) ([]*PermissionModule, error) {
	return listPermissions(ctx, s.db.Gorm, "")
}

// listPermissions is ListPermissions of the plugin pluginKey only, of the
// whole catalog for "", read through q.
func listPermissions(ctx context.Context, q *gorm.DB, pluginKey string) ([]*PermissionModule, error) {
	byModule := map[string]*PermissionModule{}
	var order []*PermissionModule
	if err := store.EachRow(permissionRows(store.G(ctx, q), pluginKey), func(rows *sql.Rows) error {
		var it PermissionItem
		var module, source string
		var pluginKey *string
		var label, desc, groupLabel, pluginName store.JSON[core.LocalizedText]
		var requires store.Strings
		var scope store.JSON[manifest.PermissionScope]
		var group PermissionGroup
		var defaults store.JSON[map[string][]string]
		if err := rows.Scan(&it.Key, &module, &label, &desc, &source, &pluginKey, &it.Risk, &requires, &it.Status,
			&it.ScopeType, &scope, &group.Key, &groupLabel, &group.Sort, &defaults, &pluginName); err != nil {
			return err
		}
		it.Label, it.Description, it.Requires, group.Label = label.V, desc.V, requires, groupLabel.V
		if it.Description == nil {
			it.Description = core.LocalizedText{}
		}
		if p, ok := corePermByKey(it.Key); ok {
			it.StepUp = p.stepUp
		}
		if ps := core.ReadPluginScope(it.ScopeType, &scope.V); ps.Resource != "" {
			it.Scope = &ps
		}
		it.Group = group.Key
		it.Defaults = qualifyDefaults(it.Key, defaults.V)
		m := byModule[module]
		if m == nil {
			m = &PermissionModule{Module: module, Source: source, PluginKey: pluginKey, Permissions: []PermissionItem{}}
			if source == "core" {
				m.Label = coreModuleLabels[module]
			} else {
				m.Label = pluginName.V
			}
			if m.Label == nil {
				m.Label = core.LocalizedText{"en": module}
			}
			byModule[module] = m
			order = append(order, m)
		}
		if group.Key != "" && !slices.ContainsFunc(m.Groups, func(g PermissionGroup) bool { return g.Key == group.Key }) {
			m.Groups = append(m.Groups, group)
		}
		m.Permissions = append(m.Permissions, it)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, m := range order {
		m.Status = m.Permissions[0].Status
		for _, p := range m.Permissions {
			if p.Status == "active" {
				m.Status = "active"
				break
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.Source != b.Source {
			return a.Source == "core"
		}
		if a.Source == "core" {
			oa, oka := coreModuleOrder[a.Module]
			ob, okb := coreModuleOrder[b.Module]
			if oka != okb {
				return oka
			}
			if oa != ob {
				return oa < ob
			}
		}
		return a.Module < b.Module
	})
	return order, nil
}

// permissionRows selects the catalog as listPermissions reads it, staged
// permissions aside, of the plugin pluginKey only unless it is "".
func permissionRows(db *gorm.DB, pluginKey string) *gorm.DB {
	q := db.Table("permissions p").Select(`p.key, p.module, p.label, p.description, p.source, p.plugin_key, p.risk, p.requires, p.status,
    p.scope_type, p.scope, p.group_key, p.group_label, p.group_sort, p.defaults, pl.name`).
		Joins("LEFT JOIN plugins pl ON pl.key = p.plugin_key").Where("p.status <> 'staged'").Order("p.source, p.module, p.group_sort, p.sort, p.key")
	if pluginKey != "" {
		q = q.Where("p.plugin_key = ?", pluginKey)
	}
	return q
}

// qualifyDefaults is a plugin permission's stored defaults (role ->
// canonical suffixes) as full keys of key, nil for none: expanded once
// here, so the console never parses suffixes.
func qualifyDefaults(key string, defaults map[string][]string) map[string][]string {
	if len(defaults) == 0 {
		return nil
	}
	out := make(map[string][]string, len(defaults))
	for role, suffixes := range defaults {
		for _, sfx := range suffixes {
			out[role] = append(out[role], withSuffix(key, sfx))
		}
	}
	return out
}

// ------------------------------------------------------------ plugin role matrix

// errPluginNotFound is the answer for a plugin the catalog holds no
// permissions of: unknown, or declaring none.
var errPluginNotFound = core.ErrNotFound.WithMessage("plugin not found")

// PluginRoleMatrix is GET/PUT /permissions/plugins/:plugin: the plugin's
// catalog module and every role's grants of it.
type PluginRoleMatrix struct {
	PluginKey string            `json:"plugin_key"`
	Module    *PermissionModule `json:"module"`
	Roles     []PluginRoleRow   `json:"roles"`
}

// PluginRoleRow is one role of a PluginRoleMatrix, as the caller may change
// it.
type PluginRoleRow struct {
	ID        int64              `json:"id"`
	Key       string             `json:"key"`
	Name      core.LocalizedText `json:"name"`
	Builtin   bool               `json:"builtin"`
	Superuser bool               `json:"superuser"`
	Editable  bool               `json:"editable"`
	Denied    string             `json:"denied,omitempty"` // editDenial
	// Revision is the role's (Role.Revision): the PUT sends it back, per
	// role, to be refused (version_conflict) when the role changed since.
	Revision int64 `json:"revision"`
	// PermissionKeys: the role's grants of this plugin, removed ones included.
	PermissionKeys []string `json:"permission_keys"`
}

// PluginRoleGrants is the plugin's catalog module (as ListPermissions lists
// it) and every role's grants of it, each with whether actorID may change
// the role (editDenial) and its revision, all read in one snapshot.
func (s *Service) PluginRoleGrants(ctx context.Context, actorID int64, pluginKey string) (*PluginRoleMatrix, error) {
	if pluginKey == "" {
		return nil, errPluginNotFound
	}
	var out *PluginRoleMatrix
	err := s.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		var err error
		out, err = s.pluginRoleGrants(ctx, tx, actorID, pluginKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) pluginRoleGrants(ctx context.Context, q *gorm.DB, actorID int64, pluginKey string) (*PluginRoleMatrix, error) {
	mods, err := listPermissions(ctx, q, pluginKey)
	if err != nil {
		return nil, err
	}
	if len(mods) == 0 {
		return nil, errPluginNotFound
	}
	roles, err := listRoles(ctx, q)
	if err != nil {
		return nil, err
	}
	actor, err := s.grantSet(ctx, q, actorID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(roles))
	for i, r := range roles {
		keys[i] = r.Key
	}
	sets, err := s.eachRolePermissions(ctx, q, keys)
	if err != nil {
		return nil, err
	}
	part := grantPart{plugin: pluginKey}
	out := &PluginRoleMatrix{PluginKey: pluginKey, Module: mods[0], Roles: make([]PluginRoleRow, 0, len(roles))}
	for _, r := range roles {
		denied, _ := editDenial(actor, sets[r.Key], r)
		out.Roles = append(out.Roles, PluginRoleRow{ID: r.ID, Key: r.Key, Name: r.Name, Builtin: r.Builtin, Superuser: r.Superuser,
			Editable: denied == "", Denied: denied, Revision: r.Revision, PermissionKeys: part.of(r.PermissionKeys)})
	}
	return out, nil
}

// SetPluginRoleGrants makes roles[k] the grants of role k of the plugin's
// permissions, for each role listed; other roles and the roles' other
// grants stay. Each role is changed as SetRolePermissions changes it
// (editableRole, setRolePart), in key order, all or none; a key of another
// plugin is foreign, and a refusal names its role (details.role). revs[k],
// when set, must be role k's revision (PluginRoleRow.Revision), else
// version_conflict.
func (s *Service) SetPluginRoleGrants(ctx context.Context, actorID int64, pluginKey string, roles map[string][]string, revs map[string]int64) (*PluginRoleMatrix, error) {
	var out *PluginRoleMatrix
	err := s.mutate(ctx, func(tx *gorm.DB) error {
		db := store.G(ctx, tx)
		found, err := gormpg.Exists(db, permissionsOfPlugin(db, pluginKey))
		if err != nil {
			return err
		}
		if !found {
			return errPluginNotFound
		}
		keys := slices.Sorted(maps.Keys(roles))
		refs, err := queryRoles(roleRefsByKeys(db, keys))
		if err != nil {
			return err
		}
		for _, k := range keys {
			i := slices.IndexFunc(refs, func(r roleRef) bool { return r.key == k })
			if i < 0 {
				return core.InvalidFields(core.FieldError{Field: "roles." + k, Code: "unknown", Message: "unknown role " + k})
			}
			var rev *int64
			if r, ok := revs[k]; ok {
				rev = &r
			}
			if err := s.setPluginRole(ctx, tx, actorID, refs[i].id, rev, pluginKey, roles[k]); err != nil {
				var ce *core.Error
				if errors.As(err, &ce) {
					return ce.WithDetails(map[string]any{"role": k})
				}
				return err
			}
		}
		// Read in the write's transaction, as mutateRole does: the
		// revisions it answers with are the ones it made.
		out, err = s.pluginRoleGrants(ctx, tx, actorID, pluginKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// setPluginRole makes keys, which must all be permissions of plugin
// pluginKey, the grants of role id (at revision rev, when set) of that
// plugin's permissions.
func (s *Service) setPluginRole(ctx context.Context, tx *gorm.DB, actorID, id int64, rev *int64, pluginKey string, keys []string) error {
	r, err := s.editableRole(ctx, tx, actorID, id, rev)
	if err != nil {
		return err
	}
	prefix := manifest.PermissionModule(pluginKey) + ":"
	if i := slices.IndexFunc(keys, func(k string) bool { return !strings.HasPrefix(k, prefix) }); i >= 0 {
		return core.InvalidFields(core.FieldError{Field: "roles." + r.Key, Code: "foreign", Message: keys[i] + " is not a permission of plugin " + pluginKey})
	}
	return s.setRolePart(ctx, tx, actorID, r, keys, grantPart{plugin: pluginKey}, map[string]any{"plugin": pluginKey, "via": "plugin_matrix"})
}

// keysNotIn returns the keys of keys that are not in of, in order (never
// nil): what a write adds, drops, or names unknown.
func keysNotIn(keys, of []string) []string {
	out := []string{}
	for _, k := range keys {
		if !slices.Contains(of, k) {
			out = append(out, k)
		}
	}
	return out
}

func dedupe(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// IsSuperuser reports whether userID holds a superuser role, regardless of
// the user's status.
func (s *Service) IsSuperuser(ctx context.Context, q *gorm.DB, userID int64) (bool, error) {
	db := store.G(ctx, q)
	return gormpg.Exists(db, superuserGrants(db, userID))
}

// superuserGrants selects the superuser roles userID holds.
func superuserGrants(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.Role{}).Where("superuser AND key IN (?)", casbinrule.RoleKeysOf(db, []int64{userID}))
}

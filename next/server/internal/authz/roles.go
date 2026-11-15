package authz

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
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
	// Version is the authz version the role was read at (authz_meta, in the
	// same statement): PUT /roles/:id/permissions sends it back to be
	// refused (version_conflict) when someone changed grants since.
	Version int64 `json:"version"`
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

var roleSelect = `
SELECT r.id, r.key, r.name, r.description, r.builtin, r.superuser, r.created_at, r.updated_at,
    COALESCE((SELECT array_agg(g.key ORDER BY g.sort, g.key) FROM (
        SELECT p.key, p.sort FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE rp.role_id = r.id
        UNION ALL
        SELECT p.key || ':' || rs.scope, p.sort FROM role_permission_scopes rs JOIN permissions p ON p.id = rs.permission_id
        WHERE rs.role_id = r.id) g), '{}'),
    COALESCE((SELECT array_agg(ar.key ORDER BY ar.key) FROM role_assignable_roles ra
        JOIN roles ar ON ar.id = ra.assignable_role_id AND NOT ar.superuser WHERE ra.role_id = r.id), '{}'),
    r.assign_custom,
    ` + roleHolders + `,
    (SELECT version FROM authz_meta WHERE id = 1)
FROM roles r`

// roleHolders counts the holders of role r, soft-deleted users aside: the
// roles page's user count, and what SyncCore judges a built-in role behind
// its rev by (held: filled in; nobody: reset).
const roleHolders = `(SELECT count(*) FROM user_roles ur JOIN users u ON u.id = ur.user_id
        WHERE ur.role_id = r.id AND u.deleted_at IS NULL)`

func scanRole(row pgx.Row) (*Role, error) {
	var r Role
	err := row.Scan(&r.ID, &r.Key, &r.Name, &r.Description, &r.Builtin, &r.Superuser, &r.CreatedAt, &r.UpdatedAt, &r.PermissionKeys, &r.AssignableRoleKeys, &r.AssignCustom, &r.UserCount, &r.Version)
	if err != nil {
		return nil, err
	}
	r.PermissionKeys = canonicalKeys(r.PermissionKeys)
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
	return listRoles(ctx, s.db.Pool)
}

func listRoles(ctx context.Context, q store.Querier) ([]*Role, error) {
	rows, err := q.Query(ctx, roleSelect+` ORDER BY r.builtin DESC, r.id`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Role, error) { return scanRole(row) })
	if out == nil {
		out = []*Role{}
	}
	return out, err
}

// GetRole returns one role or ErrNotFound.
func (s *Service) GetRole(ctx context.Context, id int64) (*Role, error) {
	return s.getRole(ctx, s.db.Pool, id)
}

func (s *Service) getRole(ctx context.Context, q store.Querier, id int64) (*Role, error) {
	r, err := scanRole(q.QueryRow(ctx, roleSelect+` WHERE r.id = $1`, id))
	if store.IsNoRows(err) {
		return nil, errRoleNotFound
	}
	return r, err
}

// editableRole locks role id inside tx, which has called Bump, and returns
// it when actorID may change it: the super admin role is frozen for everyone
// (name, description, permissions, assignable list, reset, delete); any
// other role must be within the actor's grantSet (cannotActOnRole, M5: an
// actor who could weaken a role above it could then act on that role's
// holders) and, when it is built-in or has holders, on the actor's
// assignable list: a change to the role is a change to the users holding
// it, now or later, so it takes the role the actor manages them by (§43.6;
// assign_custom never covers the actor's own roles). With the default lists
// an admin thus changes operator, supplier and user but not admin. The
// system (actor 0) passes. Every role write goes through it.
func (s *Service) editableRole(ctx context.Context, tx pgx.Tx, actorID, id int64) (*Role, error) {
	r, err := scanRole(tx.QueryRow(ctx, roleSelect+` WHERE r.id = $1 FOR UPDATE OF r`, id))
	switch {
	case store.IsNoRows(err):
		return nil, errRoleNotFound
	case err != nil:
		return nil, err
	case r.Superuser:
		return nil, errSuperuserRoleFrozen
	case actorID == 0:
		return r, nil
	}
	actor, err := s.grantSet(ctx, tx, actorID)
	if err != nil {
		return nil, err
	}
	cur, err := s.RolesPermissions(ctx, tx, []string{r.Key})
	if err != nil {
		return nil, err
	}
	switch reason, missing := editDenial(actor, cur, r); reason {
	case core.GrantDenyNotEditable:
		return nil, cannotActOnRole(ctx).WithDetails(missing.Details())
	case core.GrantDenyNotAssignable:
		return nil, notAssignable(ctx, r.Key)
	}
	return r, nil
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
	var id int64
	err := s.mutate(ctx, func(tx pgx.Tx) error {
		keys, grants, err := resolvePermissions(ctx, tx, in.PermissionKeys)
		if err != nil {
			return err
		}
		assign, err := assignableRoles(ctx, tx, in.AssignableRoleKeys)
		if err != nil {
			return err
		}
		grant, err := grantOf(ctx, tx, in.Key, keys, roleKeysOf(assign), in.AssignCustom)
		if err != nil {
			return err
		}
		if err := s.covers(ctx, tx, actorID, grant, cannotGrant(ctx)); err != nil {
			return err
		}
		if err := s.stepUpCritical(ctx, grants, keys); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO roles (key, name, description) VALUES ($1, $2, $3) RETURNING id`,
			in.Key, in.Name, in.Description).Scan(&id)
		if store.IsUniqueViolation(err, "") {
			return core.ErrConflict.WithMessage("role key already exists")
		}
		if err != nil {
			return err
		}
		if err := writeGrants(ctx, tx, id, grants, grantPart{}); err != nil {
			return err
		}
		if err := setRoleAssignable(ctx, tx, id, assign, in.AssignCustom); err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "role.create", "role", id, map[string]any{"key": in.Key, "permission_keys": keys,
			"assignable_role_keys": roleKeysOf(assign), "assign_custom": in.AssignCustom})
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
}

// UpdateRoleInput is the body of PATCH /roles/:id.
type UpdateRoleInput struct {
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
	err := s.mutate(ctx, func(tx pgx.Tx) error {
		r, err := s.editableRole(ctx, tx, actorID, id)
		if err != nil {
			return err
		}
		if in.Key != nil && *in.Key != r.Key {
			return errRoleKeyImmutable(ctx)
		}
		if _, err := tx.Exec(ctx, `UPDATE roles SET name = COALESCE($2, name), description = COALESCE($3, description), updated_at = now() WHERE id = $1`,
			id, nilIfEmpty(in.Name), nilIfEmpty(in.Description)); err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "role.update", "role", id, in)
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
}

func nilIfEmpty(t core.LocalizedText) any {
	if t == nil {
		return nil
	}
	return t
}

// DeleteRole deletes a custom role actorID may edit (editableRole: while it
// has holders, deleting it revokes it, and the role must be on the actor's
// assignable list); its assignments cascade.
func (s *Service) DeleteRole(ctx context.Context, actorID, id int64) error {
	return s.mutate(ctx, func(tx pgx.Tx) error {
		r, err := s.editableRole(ctx, tx, actorID, id)
		if err != nil {
			return err
		}
		if r.Builtin {
			return errBuiltinRole
		}
		if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE id = $1`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "role.delete", "role", id, map[string]any{"key": r.Key})
	})
}

// ResetRole restores a built-in role actorID may edit (editableRole) to its
// defaults (builtinRoles): name, description and what resetDefaults
// writes. The actor must hold every default, as for SetRolePermissions and
// SetRoleAssignable; what the reset drops is within the actor already
// (editableRole), so it cannot weaken a role above the actor (C-L2).
func (s *Service) ResetRole(ctx context.Context, actorID, id int64) (*Role, error) {
	err := s.mutate(ctx, func(tx pgx.Tx) error {
		r, err := s.editableRole(ctx, tx, actorID, id)
		if err != nil {
			return err
		}
		def, ok := builtinRoleByKey(r.Key)
		if !r.Builtin || !ok {
			return errNotBuiltinRole
		}
		if _, err := tx.Exec(ctx, `UPDATE roles SET name = $2, description = $3 WHERE id = $1`, id, def.name, def.description); err != nil {
			return err
		}
		return s.resetDefaults(ctx, tx, actorID, r, def, map[string]any{})
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
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
func (s *Service) resetDefaults(ctx context.Context, tx pgx.Tx, actorID int64, r *Role, def builtinRole, detail map[string]any) error {
	keys, grants, err := resolvePermissions(ctx, tx, def.defaultPermissions())
	if err != nil {
		return err
	}
	assign, err := queryRoles(ctx, tx, `SELECT id, key, superuser FROM roles WHERE key = ANY($1) AND builtin AND NOT superuser ORDER BY key`, def.assignable)
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
	if _, err := tx.Exec(ctx, `UPDATE roles SET defaults_rev = $2 WHERE id = $1`, r.ID, def.rev); err != nil {
		return err
	}
	if err := writeGrants(ctx, tx, r.ID, grants, grantPart{coreOnly: true}); err != nil {
		return err
	}
	after, err := s.getRole(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	detail["key"], detail["before"], detail["after"] = r.Key, roleDetail(r), roleDetail(after)
	return s.audit(ctx, tx, actorID, "role.reset", "role", r.ID, detail)
}

// roleDetail is what a role grants, for an audit's before and after.
func roleDetail(r *Role) map[string]any {
	return map[string]any{"permission_keys": r.PermissionKeys, "assignable_role_keys": r.AssignableRoleKeys, "assign_custom": r.AssignCustom}
}

// SetRolePermissions replaces the permissions of a role actorID may edit
// (editableRole), as setRolePart does. version, when set, must be the authz
// version the editor read (Role.Version), else version_conflict.
func (s *Service) SetRolePermissions(ctx context.Context, actorID, id int64, keys []string, version *int64) (*Role, error) {
	err := s.mutateAt(ctx, version, func(tx pgx.Tx) error {
		r, err := s.editableRole(ctx, tx, actorID, id)
		if err != nil {
			return err
		}
		return s.setRolePart(ctx, tx, actorID, r, keys, grantPart{}, map[string]any{})
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
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
func (s *Service) setRolePart(ctx context.Context, tx pgx.Tx, actorID int64, r *Role, keys []string, part grantPart, detail map[string]any) error {
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
	if err := writeGrants(ctx, tx, r.ID, grants, part); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE roles SET updated_at = now() WHERE id = $1`, r.ID); err != nil {
		return err
	}
	after, err := s.getRole(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	before, now := part.of(r.PermissionKeys), part.of(after.PermissionKeys)
	detail["permission_keys"], detail["added"], detail["removed"] = live, added, append([]string{}, addedKeys(before, now)...)
	if part.plugin != "" {
		detail["before"], detail["after"] = before, now
	}
	return s.audit(ctx, tx, actorID, "role.permissions", "role", r.ID, detail)
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
}

// SetRoleAssignable replaces the assignable list of a role actorID may edit
// (editableRole). The actor must be able to assign what is added (the
// "every custom role" flag included); what the role keeps is within it
// already.
func (s *Service) SetRoleAssignable(ctx context.Context, actorID, id int64, in AssignableInput) (*Role, error) {
	err := s.mutate(ctx, func(tx pgx.Tx) error {
		r, err := s.editableRole(ctx, tx, actorID, id)
		if err != nil {
			return err
		}
		assign, err := assignableRoles(ctx, tx, in.RoleKeys)
		if err != nil {
			return err
		}
		keys := roleKeysOf(assign)
		added := addedKeys(keys, r.AssignableRoleKeys)
		grant, err := grantOf(ctx, tx, r.Key, nil, added, in.AssignCustom && !r.AssignCustom)
		if err != nil {
			return err
		}
		if err := s.covers(ctx, tx, actorID, grant, cannotGrant(ctx)); err != nil {
			return err
		}
		if err := setRoleAssignable(ctx, tx, id, assign, in.AssignCustom); err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "role.assignable", "role", id, map[string]any{"role_keys": keys, "assign_custom": in.AssignCustom, "added": added})
	})
	if err != nil {
		return nil, err
	}
	return s.GetRole(ctx, id)
}

// assignableRoles resolves keys for an assignable list: each must exist and
// none may be a superuser role, which is only ever transferred.
func assignableRoles(ctx context.Context, q store.Querier, keys []string) ([]roleRef, error) {
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
func grantOf(ctx context.Context, q store.Querier, roleKey string, perms, roles []string, custom bool) (core.PermissionSet, error) {
	set := core.PermissionSetOf(perms...)
	set.Assignable = core.PermissionSetOf(roles...).Keys
	set.AssignCustom = custom
	if !custom {
		return set, nil
	}
	var keys []string
	if err := q.QueryRow(ctx, `SELECT COALESCE(array_agg(key), '{}') FROM roles WHERE NOT builtin AND key <> $1`, roleKey).Scan(&keys); err != nil {
		return core.PermissionSet{}, err
	}
	for _, k := range keys {
		set.Assignable[k] = struct{}{}
	}
	return set, nil
}

// setRoleAssignable makes roles, plus every custom role when custom, the
// role's assignable list.
func setRoleAssignable(ctx context.Context, tx pgx.Tx, roleID int64, roles []roleRef, custom bool) error {
	ids := roleIDsOf(roles)
	if _, err := tx.Exec(ctx, `DELETE FROM role_assignable_roles WHERE role_id = $1 AND assignable_role_id <> ALL($2)`, roleID, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO role_assignable_roles (role_id, assignable_role_id) SELECT $1, unnest($2::bigint[]) ON CONFLICT DO NOTHING`, roleID, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE roles SET assign_custom = $2, updated_at = now() WHERE id = $1`, roleID, custom)
	return err
}

// ------------------------------------------------------------ user roles (used by iam)

// SetUserRoles replaces a user's roles inside tx, bumping the authz version
// (call Committed with the returned version after commit). actorID is the
// acting user (0 = system); checkAssignment judges what the change adds and
// removes, and a change is audited as user.roles.
func (s *Service) SetUserRoles(ctx context.Context, tx pgx.Tx, actorID, userID int64, roleKeys []string) (int64, error) {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return 0, err
	}
	want, err := resolveRoles(ctx, tx, dedupe(roleKeys))
	if err != nil {
		return 0, err
	}
	cur, err := queryRoles(ctx, tx, `SELECT r.id, r.key, r.superuser FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1 ORDER BY r.key`, userID)
	if err != nil {
		return 0, err
	}
	added, removed := diffRoles(want, cur), diffRoles(cur, want)
	if len(added)+len(removed) == 0 {
		return v, nil
	}
	if err := s.checkAssignment(ctx, tx, actorID, userID, len(cur) == 0, added, removed); err != nil {
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
// in turn, must be within the actor's grantSet. The system (actor 0) may
// grant the initial super admin, never revoke one.
func (s *Service) checkAssignment(ctx context.Context, q store.Querier, actorID, userID int64, roleless bool, added, removed []roleRef) error {
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
	return within(actor, grant, cannotGrant(ctx))
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

// queryRoles runs sql, which selects id, key and superuser of roles.
func queryRoles(ctx context.Context, q store.Querier, sql string, args ...any) ([]roleRef, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (roleRef, error) {
		var r roleRef
		err := row.Scan(&r.id, &r.key, &r.superuser)
		return r, err
	})
}

// resolveRoles returns the roles of keys, failing on unknown ones.
func resolveRoles(ctx context.Context, q store.Querier, keys []string) ([]roleRef, error) {
	roles, err := queryRoles(ctx, q, `SELECT id, key, superuser FROM roles WHERE key = ANY($1) ORDER BY key`, keys)
	if err != nil {
		return nil, err
	}
	if unknown := addedKeys(keys, roleKeysOf(roles)); len(unknown) > 0 {
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
func writeUserRoles(ctx context.Context, tx pgx.Tx, userID int64, roles []roleRef) error {
	ids := roleIDsOf(roles)
	if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1 AND role_id <> ALL($2)`, userID, ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO user_roles (user_id, role_id) SELECT $1, unnest($2::bigint[]) ON CONFLICT DO NOTHING`, userID, ids)
	return err
}

// EnsureNotLastSuperAdmin fails with conflict when userID is a superuser and
// no other active superuser exists. iam calls it inside the transaction that
// disables or deletes a user, after Bump (which serializes the check).
func (s *Service) EnsureNotLastSuperAdmin(ctx context.Context, tx pgx.Tx, userID int64) error {
	var su bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1 AND r.superuser)`, userID).Scan(&su); err != nil {
		return err
	}
	if !su {
		return nil
	}
	return s.ensureOtherSuperuser(ctx, tx, userID)
}

func (s *Service) ensureOtherSuperuser(ctx context.Context, tx pgx.Tx, userID int64) error {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(DISTINCT u.id) FROM users u
        JOIN user_roles ur ON ur.user_id = u.id JOIN roles r ON r.id = ur.role_id
        WHERE r.superuser AND u.deleted_at IS NULL AND u.status = 'active' AND u.id <> $1`, userID).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return errLastSuperAdmin
	}
	return nil
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
	q := s.db.Pool
	actor, err := s.grantSet(ctx, q, actorID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT key, name, builtin FROM roles WHERE NOT superuser ORDER BY builtin DESC, id`)
	if err != nil {
		return nil, err
	}
	roles, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AssignableRole, error) {
		var r AssignableRole
		err := row.Scan(&r.Key, &r.Name, &r.Builtin)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	out := []AssignableRole{}
	for _, r := range roles {
		if !actor.CanAssign(r.Key) {
			continue
		}
		grant, err := s.RolesPermissions(ctx, q, []string{r.Key})
		if err != nil {
			return nil, err
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
func (s *Service) ListPermissions(ctx context.Context) ([]*PermissionModule, error) {
	return listPermissions(ctx, s.db.Pool, "")
}

// listPermissions is ListPermissions of the plugin pluginKey only, of the
// whole catalog for "", read through q.
func listPermissions(ctx context.Context, q store.Querier, pluginKey string) ([]*PermissionModule, error) {
	rows, err := q.Query(ctx, `
SELECT p.key, p.module, p.label, p.description, p.source, p.plugin_key, p.risk, p.requires, p.status, p.scope_type, p.scope,
    p.group_key, p.group_label, p.group_sort, p.defaults, pl.name
FROM permissions p LEFT JOIN plugins pl ON pl.key = p.plugin_key
WHERE $1 = '' OR p.plugin_key = $1
ORDER BY p.source, p.module, p.group_sort, p.sort, p.key`, pluginKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byModule := map[string]*PermissionModule{}
	var order []*PermissionModule
	for rows.Next() {
		var it PermissionItem
		var module, source string
		var pluginKey *string
		var scope manifest.PermissionScope
		var group PermissionGroup
		var defaults map[string][]string
		var pluginName core.LocalizedText
		if err := rows.Scan(&it.Key, &module, &it.Label, &it.Description, &source, &pluginKey, &it.Risk, &it.Requires, &it.Status,
			&it.ScopeType, &scope, &group.Key, &group.Label, &group.Sort, &defaults, &pluginName); err != nil {
			return nil, err
		}
		if it.Description == nil {
			it.Description = core.LocalizedText{}
		}
		if p, ok := corePermByKey(it.Key); ok {
			it.StepUp = p.stepUp
		}
		if ps := core.ReadPluginScope(it.ScopeType, &scope); ps.Resource != "" {
			it.Scope = &ps
		}
		it.Group = group.Key
		it.Defaults = qualifyDefaults(it.Key, defaults)
		m := byModule[module]
		if m == nil {
			m = &PermissionModule{Module: module, Source: source, PluginKey: pluginKey, Permissions: []PermissionItem{}}
			if source == "core" {
				m.Label = coreModuleLabels[module]
			} else {
				m.Label = pluginName
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
	}
	if err := rows.Err(); err != nil {
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
	// Version is the authz version all of it was read at: the PUT sends it
	// back to be refused (version_conflict) when grants changed since.
	Version int64 `json:"version"`
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
	// PermissionKeys: the role's grants of this plugin, removed ones included.
	PermissionKeys []string `json:"permission_keys"`
}

// PluginRoleGrants is the plugin's catalog module (as ListPermissions lists
// it) and every role's grants of it, each with whether actorID may change
// the role (editDenial), all read in one snapshot whose authz version it
// carries.
func (s *Service) PluginRoleGrants(ctx context.Context, actorID int64, pluginKey string) (*PluginRoleMatrix, error) {
	if pluginKey == "" {
		return nil, errPluginNotFound
	}
	var out *PluginRoleMatrix
	v, err := s.readSnapshot(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.pluginRoleGrants(ctx, tx, actorID, pluginKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	out.Version = v
	return out, nil
}

func (s *Service) pluginRoleGrants(ctx context.Context, q store.Querier, actorID int64, pluginKey string) (*PluginRoleMatrix, error) {
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
			Editable: denied == "", Denied: denied, PermissionKeys: part.of(r.PermissionKeys)})
	}
	return out, nil
}

// SetPluginRoleGrants makes roles[k] the grants of role k of the plugin's
// permissions, for each role listed; other roles and the roles' other
// grants stay. Each role is changed as SetRolePermissions changes it
// (editableRole, setRolePart), in key order, all or none; a key of another
// plugin is foreign, and a refusal names its role (details.role). version,
// when set, must be the matrix's (PluginRoleMatrix.Version), else
// version_conflict.
func (s *Service) SetPluginRoleGrants(ctx context.Context, actorID int64, pluginKey string, roles map[string][]string, version *int64) (*PluginRoleMatrix, error) {
	err := s.mutateAt(ctx, version, func(tx pgx.Tx) error {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM permissions WHERE plugin_key = $1)`, pluginKey).Scan(&found); err != nil {
			return err
		}
		if !found {
			return errPluginNotFound
		}
		keys := slices.Sorted(maps.Keys(roles))
		refs, err := queryRoles(ctx, tx, `SELECT id, key, superuser FROM roles WHERE key = ANY($1)`, keys)
		if err != nil {
			return err
		}
		for _, k := range keys {
			i := slices.IndexFunc(refs, func(r roleRef) bool { return r.key == k })
			if i < 0 {
				return core.InvalidFields(core.FieldError{Field: "roles." + k, Code: "unknown", Message: "unknown role " + k})
			}
			if err := s.setPluginRole(ctx, tx, actorID, refs[i].id, pluginKey, roles[k]); err != nil {
				var ce *core.Error
				if errors.As(err, &ce) {
					return ce.WithDetails(map[string]any{"role": k})
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.PluginRoleGrants(ctx, actorID, pluginKey)
}

// setPluginRole makes keys, which must all be permissions of plugin
// pluginKey, the grants of role id of that plugin's permissions.
func (s *Service) setPluginRole(ctx context.Context, tx pgx.Tx, actorID, id int64, pluginKey string, keys []string) error {
	r, err := s.editableRole(ctx, tx, actorID, id)
	if err != nil {
		return err
	}
	prefix := manifest.PermissionModule(pluginKey) + ":"
	if i := slices.IndexFunc(keys, func(k string) bool { return !strings.HasPrefix(k, prefix) }); i >= 0 {
		return core.InvalidFields(core.FieldError{Field: "roles." + r.Key, Code: "foreign", Message: keys[i] + " is not a permission of plugin " + pluginKey})
	}
	return s.setRolePart(ctx, tx, actorID, r, keys, grantPart{plugin: pluginKey}, map[string]any{"plugin": pluginKey, "via": "plugin_matrix"})
}

// addedKeys returns the keys in want that are not in have, in want order.
func addedKeys(want, have []string) []string {
	var out []string
	for _, k := range want {
		if !slices.Contains(have, k) {
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
func (s *Service) IsSuperuser(ctx context.Context, q store.Querier, userID int64) (bool, error) {
	var su bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = $1 AND r.superuser)`, userID).Scan(&su)
	return su, err
}

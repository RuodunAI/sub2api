package authz

import (
	"context"
	"slices"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

var (
	// ErrNotTheSuperAdmin is the denial of a transfer by anyone but the super
	// admin.
	ErrNotTheSuperAdmin = core.ErrPermissionDenied.WithMessage("only the super admin can transfer the super admin role")

	errTransferToSelf         = transferTargetError("self", "you already are the super admin")
	errTransferTargetNotFound = transferTargetError("not_found", "user not found")
	errTransferTargetInactive = transferTargetError("inactive", "the user is disabled")
)

func transferTargetError(code, message string) *core.Error {
	return core.InvalidFields(core.FieldError{Field: "target_user_id", Code: code, Message: message})
}

// TransferSuperAdmin hands the super admin role to targetID inside tx, the
// only way it changes hands; every other holder becomes an admin, except a
// deleted one, which only loses the role (as 0097 does). actorID must hold
// it; 0 is the server CLI, which also repairs a database with no or several
// holders. It returns the bumped version (call Committed after commit) and
// the demoted users.
func (s *Service) TransferSuperAdmin(ctx context.Context, tx *gorm.DB, actorID, targetID int64) (int64, []int64, error) {
	return s.transferSuperAdmin(ctx, tx, policyHolders{}, actorID, targetID)
}

// TransferSuperAdminLegacy is TransferSuperAdmin for the server CLI on a
// database the migrations stopped before 0100 (LegacySchema), whose 0097
// asks for it: the holders are user_roles rows there. Besides authz_meta
// and the audit it reads and writes them and users only.
func (s *Service) TransferSuperAdminLegacy(ctx context.Context, tx *gorm.DB, actorID, targetID int64) (int64, []int64, error) {
	return s.transferSuperAdmin(ctx, tx, legacyHolders{}, actorID, targetID)
}

func (s *Service) transferSuperAdmin(ctx context.Context, tx *gorm.DB, h holderStore, actorID, targetID int64) (int64, []int64, error) {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	db := store.G(ctx, tx)
	holders, err := h.lock(db)
	if err != nil {
		return 0, nil, err
	}
	if actorID != 0 && !slices.Contains(holders, actorID) {
		return 0, nil, ErrNotTheSuperAdmin
	}
	if err := checkTransferTarget(ctx, tx, actorID, targetID); err != nil {
		return 0, nil, err
	}
	// Revoke before grant: the one-holder index never sees two.
	demoted := slices.DeleteFunc(slices.Clone(holders), func(id int64) bool { return id == targetID })
	if err := h.revoke(db, demoted); err != nil {
		return 0, nil, err
	}
	live, err := undeleted(db, demoted)
	if err != nil {
		return 0, nil, err
	}
	if err := h.grant(db, RoleAdmin, live); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(holders, targetID) {
		if err := h.grant(db, RoleSuperAdmin, []int64{targetID}); err != nil {
			return 0, nil, err
		}
	}
	via := "console"
	if actorID == 0 {
		via = "cli"
	}
	return v, demoted, s.audit(ctx, tx, actorID, "superadmin.transfer", "user", targetID, map[string]any{"from": holders, "to": targetID, "via": via})
}

// undeleted returns the users of ids not soft-deleted, by id (never nil).
func undeleted(db *gorm.DB, ids []int64) ([]int64, error) {
	out := []int64{}
	err := db.Model(&model.User{}).Where("id = ANY(?) AND deleted_at IS NULL", store.Int64s(ids)).Order("id").Pluck("id", &out).Error
	return out, err
}

// holderStore is where the super admin role's holders are: casbin_rule, or
// user_roles on a database the 0.1.290 migrations have not reached past
// 0097, whose check of one holder points the operator at the CLI before
// 0100 replaces the table (TransferSuperAdminLegacy).
type holderStore interface {
	// lock returns the holders, by id, and locks their rows.
	lock(db *gorm.DB) ([]int64, error)
	revoke(db *gorm.DB, userIDs []int64) error
	grant(db *gorm.DB, role string, userIDs []int64) error
}

// casbinRuleMigration moves the roles from user_roles to casbin_rule.
const casbinRuleMigration = "0100_casbin_rule.sql"

// LegacySchema reports whether the database db reads is one the
// migrations stopped before 0100 (schema_migrations has no
// casbinRuleMigration, so user_roles still holds the roles): where the
// server CLI recovers a super admin for 0097 (TransferSuperAdminLegacy),
// and nothing else of this core runs. A user_roles table found after 0100
// is a stray one and changes nothing.
func LegacySchema(ctx context.Context, db *gorm.DB) (bool, error) {
	applied, err := store.CoreTracker{}.Applied(ctx, db)
	if err != nil {
		return false, err
	}
	_, migrated := applied[casbinRuleMigration]
	return !migrated, nil
}

// policyHolders are casbin_rule's g rows of the super admin role (the one
// superuser role, roles_superuser_key).
type policyHolders struct{}

func (policyHolders) lock(db *gorm.DB) ([]int64, error) {
	return casbinrule.LockMembers(db, RoleSuperAdmin)
}

func (policyHolders) revoke(db *gorm.DB, userIDs []int64) error {
	return casbinrule.RemoveMembers(db, RoleSuperAdmin, userIDs)
}

func (policyHolders) grant(db *gorm.DB, role string, userIDs []int64) error {
	return casbinrule.AddMembers(db, role, userIDs)
}

// legacyHolders are the user_roles rows of the superuser role, on a schema
// before migration 0100 only.
type legacyHolders struct{}

// legacyUserRole is a row of user_roles, the users' roles before migration
// 0100 (no generated model: the table is gone after it).
type legacyUserRole struct {
	UserID int64
	RoleID int64
}

// TableName implements gorm's schema.Tabler.
func (legacyUserRole) TableName() string { return "user_roles" }

// superuserRoleIDs selects the ids of the superuser roles.
func superuserRoleIDs(db *gorm.DB) *gorm.DB {
	return db.Model(&model.Role{}).Select("id").Where("superuser")
}

func (legacyHolders) lock(db *gorm.DB) ([]int64, error) {
	if err := store.RequireTx(db); err != nil {
		return nil, err
	}
	ids := []int64{}
	err := db.Model(&legacyUserRole{}).Where("role_id IN (?)", superuserRoleIDs(db)).Order("user_id").Clauses(store.ForUpdate()).Pluck("user_id", &ids).Error
	return slices.Compact(ids), err
}

func (legacyHolders) revoke(db *gorm.DB, userIDs []int64) error {
	return db.Where("user_id = ANY(?) AND role_id IN (?)", store.Int64s(userIDs), superuserRoleIDs(db)).Delete(&legacyUserRole{}).Error
}

func (legacyHolders) grant(db *gorm.DB, role string, userIDs []int64) error {
	var ids []int64
	if err := db.Model(&model.Role{}).Where("key = ?", role).Pluck("id", &ids).Error; err != nil || len(ids) == 0 || len(userIDs) == 0 {
		return err
	}
	rows := make([]legacyUserRole, len(userIDs))
	for i, u := range userIDs {
		rows[i] = legacyUserRole{UserID: u, RoleID: ids[0]}
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

// checkTransferTarget fails unless targetID is an active, undeleted user
// other than the acting super admin, and locks its row.
func checkTransferTarget(ctx context.Context, tx *gorm.DB, actorID, targetID int64) error {
	if err := store.RequireTx(tx); err != nil {
		return err
	}
	if actorID != 0 && targetID == actorID {
		return errTransferToSelf
	}
	var active bool
	err := lockTransferTarget(store.G(ctx, tx), targetID).Row().Scan(&active)
	switch {
	case store.IsNoRows(err):
		return errTransferTargetNotFound
	case err != nil:
		return err
	case !active:
		return errTransferTargetInactive
	}
	return nil
}

// lockTransferTarget selects whether the undeleted user userID is active
// and locks its row.
func lockTransferTarget(db *gorm.DB, userID int64) *gorm.DB {
	return db.Model(&model.User{}).Select("status = 'active'").Where("id = ? AND deleted_at IS NULL", userID).Clauses(store.ForUpdate())
}

package authz

import (
	"context"
	"slices"

	"gorm.io/gorm"

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
// it; 0 is the server CLI, which also repairs a database with no holder.
// It returns the bumped version (call Committed after commit) and the
// demoted users.
func (s *Service) TransferSuperAdmin(ctx context.Context, tx *gorm.DB, actorID, targetID int64) (int64, []int64, error) {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	db := store.G(ctx, tx)
	holders, err := casbinrule.LockMembers(db, RoleSuperAdmin)
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
	if err := casbinrule.RemoveMembers(db, RoleSuperAdmin, demoted); err != nil {
		return 0, nil, err
	}
	live, err := undeleted(db, demoted)
	if err != nil {
		return 0, nil, err
	}
	if err := casbinrule.AddMembers(db, RoleAdmin, live); err != nil {
		return 0, nil, err
	}
	if !slices.Contains(holders, targetID) {
		if err := casbinrule.AddMembers(db, RoleSuperAdmin, []int64{targetID}); err != nil {
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

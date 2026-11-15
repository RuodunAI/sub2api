package authz

import (
	"context"
	"slices"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
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
// deleted one, which only loses the role (as 0098 does). actorID must hold
// it; 0 is the server CLI, which also repairs a database with no or several
// holders. It returns the bumped version (call Committed after
// commit) and the demoted users. Only roles, user_roles and users are read
// and written, so the CLI runs on any schema it may meet.
func (s *Service) TransferSuperAdmin(ctx context.Context, tx *gorm.DB, actorID, targetID int64) (int64, []int64, error) {
	v, err := s.Bump(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	holders, err := superAdmins(ctx, tx)
	if err != nil {
		return 0, nil, err
	}
	if actorID != 0 && !slices.Contains(holders, actorID) {
		return 0, nil, ErrNotTheSuperAdmin
	}
	if err := checkTransferTarget(ctx, tx, actorID, targetID); err != nil {
		return 0, nil, err
	}
	// Delete before insert, so a one-holder constraint never sees two.
	demoted := slices.DeleteFunc(slices.Clone(holders), func(id int64) bool { return id == targetID })
	db := store.G(ctx, tx)
	if err := revokeSuperuser(db, demoted).Error; err != nil {
		return 0, nil, err
	}
	if err := grantAdmin(db, demoted).Error; err != nil {
		return 0, nil, err
	}
	if !slices.Contains(holders, targetID) {
		if err := grantSuperuser(db, targetID).Error; err != nil {
			return 0, nil, err
		}
	}
	via := "console"
	if actorID == 0 {
		via = "cli"
	}
	return v, demoted, s.audit(ctx, tx, actorID, "superadmin.transfer", "user", targetID, map[string]any{"from": holders, "to": targetID, "via": via})
}

// revokeSuperuser revokes the superuser roles of userIDs.
func revokeSuperuser(db *gorm.DB, userIDs []int64) *gorm.DB {
	return db.Exec(`DELETE FROM user_roles ur USING roles r WHERE r.id = ur.role_id AND r.superuser AND ur.user_id = ANY(?)`, store.Int64s(userIDs))
}

// grantAdmin assigns the admin role to the undeleted users of userIDs.
func grantAdmin(db *gorm.DB, userIDs []int64) *gorm.DB {
	return db.Exec(`INSERT INTO user_roles (user_id, role_id) SELECT u.id, r.id FROM users u, roles r
		WHERE u.id = ANY(?) AND u.deleted_at IS NULL AND r.key = ? ON CONFLICT DO NOTHING`, store.Int64s(userIDs), RoleAdmin)
}

// grantSuperuser assigns the superuser roles to userID.
func grantSuperuser(db *gorm.DB, userID int64) *gorm.DB {
	return db.Exec(`INSERT INTO user_roles (user_id, role_id) SELECT ?, id FROM roles WHERE superuser`, userID)
}

// superAdmins locks the rows of superuser roles and returns their holders
// (never nil).
func superAdmins(ctx context.Context, tx *gorm.DB) ([]int64, error) {
	ids := []int64{}
	if err := lockSuperAdmins(store.G(ctx, tx)).Scan(&ids).Error; err != nil {
		return nil, err
	}
	return slices.Compact(ids), nil
}

// lockSuperAdmins selects the holders of superuser roles, by id, and locks
// their user_roles rows.
func lockSuperAdmins(db *gorm.DB) *gorm.DB {
	return db.Raw(`SELECT ur.user_id FROM user_roles ur JOIN roles r ON r.id = ur.role_id AND r.superuser ORDER BY 1 FOR UPDATE OF ur`)
}

// checkTransferTarget fails unless targetID is an active, undeleted user
// other than the acting super admin, and locks its row.
func checkTransferTarget(ctx context.Context, tx *gorm.DB, actorID, targetID int64) error {
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
	return db.Raw(`SELECT status = 'active' FROM users WHERE id = ? AND deleted_at IS NULL FOR UPDATE`, userID)
}

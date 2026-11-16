package authz

import (
	"cmp"
	"context"
	"slices"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// CheckOwner implements core.OwnerGuard (CONTRACTS §102.3). The caller has
// bumped naming userID in tx: the owner graph it reads cannot move under it,
// and the bump tells every node's teams of the move.
func (s *Service) CheckOwner(ctx context.Context, tx *gorm.DB, actorID, userID int64, create bool, owner *int64) (int64, error) {
	if err := store.RequireTx(tx); err != nil {
		return 0, err
	}
	db := store.G(ctx, tx)
	var from int64
	key := perm.UserCreate
	if !create {
		key = perm.UserUpdate
		var err error
		if from, err = ownerOf(db, userID); err != nil || owner == nil || *owner == from {
			return from, err
		}
	}
	all, team, err := s.namesOwners(ctx, actorID, key)
	if err != nil {
		return 0, err
	}
	to := from
	switch {
	case owner != nil && !all && !(team && *owner == actorID):
		return 0, core.ErrPermissionDenied.WithMessage(t(ctx,
			"you can only make yourself the owner", "你只能把自己设为 owner"))
	case owner != nil:
		to = *owner
	case team && !all:
		to = actorID
	}
	if to == from {
		return to, nil
	}
	if to != 0 {
		if err := s.ownerFits(ctx, db, userID, to); err != nil {
			return 0, err
		}
	}
	targets := []int64{from, to}
	if !create {
		targets = append(targets, userID)
	}
	targets = slices.DeleteFunc(targets, func(id int64) bool { return id == 0 || id == actorID })
	if err := s.canActOnUsers(ctx, tx, actorID, core.ActOnIdentity, targets...); err != nil {
		return 0, err
	}
	return to, s.audit(ctx, tx, actorID, "user.owner", "user", userID, map[string]any{"from": idOrNil(from), "to": idOrNil(to)})
}

// namesOwners reports whether actorID, in this request, may name any owner
// (it holds key for every user; the system does) and whether it holds key
// for its team.
func (s *Service) namesOwners(ctx context.Context, actorID int64, key perm.Key) (all, team bool, err error) {
	if actorID == 0 {
		return true, false, nil
	}
	if all, err = s.Can(ctx, actorID, string(key)); all || err != nil {
		return all, false, err
	}
	team, err = s.Can(ctx, actorID, string(perm.Pattern{Action: key, Scope: perm.Scope{Team: true}}.Key()))
	return false, team, err
}

// ownerFits fails, with a 400 on owner_id, unless owner may own userID: it is
// another live user, not below userID, its chain stays within
// core.MaxTeamDepth levels with userID's users below it, and it manages
// userID by its roles.
func (s *Service) ownerFits(ctx context.Context, db *gorm.DB, userID, owner int64) error {
	if owner == userID {
		return badOwner(ctx, "self")
	}
	live, err := gormpg.Exists(db, db.Model(&model.User{}).Where("id = ? AND deleted_at IS NULL", owner))
	if err != nil || !live {
		return cmp.Or(err, badOwner(ctx, "not_found"))
	}
	below, levels, err := subtree(db, userID)
	if err != nil {
		return err
	}
	if _, ok := slices.BinarySearch(below, owner); ok {
		return badOwner(ctx, "cycle")
	}
	above, err := depth(db, owner)
	if err != nil {
		return err
	}
	if above+1+levels > core.MaxTeamDepth {
		return badOwner(ctx, "too_deep")
	}
	target, roles, err := s.usersRoles(ctx, db, []int64{userID})
	if err != nil {
		return err
	}
	return s.ownerManages(ctx, db, owner, target, roles)
}

// ownerManages fails with owner_cannot_manage unless owner manages, by its
// roles, a user holding roles, which grant target (manageDenial, §5.4 of
// the plan): an owner reading its team's accounts must never reach those
// of a user it could not have made.
func (s *Service) ownerManages(ctx context.Context, q *gorm.DB, owner int64, target core.PermissionSet, roles []string) error {
	set, err := s.usersRoleSet(ctx, q, []int64{owner})
	if err != nil {
		return err
	}
	if manageDenial(ctx, set, target, roles) != nil {
		return badOwner(ctx, "owner_cannot_manage")
	}
	return nil
}

// badOwner is the 400 of an owner_id refused for code.
func badOwner(ctx context.Context, code string) error {
	msg := map[string][2]string{
		"self":                {"a user cannot be its own owner", "用户不能是自己的 owner"},
		"not_found":           {"owner_id must be a user that is not deleted", "owner_id 必须是未删除的用户"},
		"cycle":               {"the owner is below this user", "owner 在该用户的下级之中"},
		"too_deep":            {"the owner chain would be more than 8 levels deep", "owner 链会超过 8 层"},
		"owner_cannot_manage": {"the owner cannot manage this user by its roles; change the owner first", "owner 按角色管理不了该用户，请先修改 owner"},
	}[code]
	return core.InvalidFields(core.FieldError{Field: "owner_id", Code: code, Message: t(ctx, msg[0], msg[1])})
}

// idOrNil is id for an audit detail: nil for 0 (none).
func idOrNil(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

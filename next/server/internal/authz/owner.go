package authz

import (
	"context"
	"errors"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/runtime-contract/gormpg"
	"github.com/Sub2API-Devs/sup2api/next/sdk/hierarchy"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/authz/casbinrule"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// SetOwner implements core.OwnerGuard (CONTRACTS §103.3): the one writer
// of users.owner_id and owner_path, which core.UserTree moves together
// with the paths below the user. It takes the authz lock (lockVersion), so
// the owners it reads cannot move under it and role writes judge owners
// (ownersKeepManaging) one at a time with it. Whether the actor may name
// the owner is settled before anything about the owner is read: a team
// holder learns nothing of a user outside its team. On an update the actor
// must manage the user as it stands under the lock, before the move: after
// it the user is in the team of a holder naming itself, so a check then
// would pass whatever happened since the caller's own read. A refusal
// after the move fails the caller's transaction, which takes the move
// back.
func (s *Service) SetOwner(ctx context.Context, tx *gorm.DB, actorID, userID int64, create bool, owner *int64) (int64, error) {
	if err := store.RequireTx(tx); err != nil {
		return 0, err
	}
	db := store.G(ctx, tx)
	if err := lockVersion(db); err != nil {
		return 0, err
	}
	down, err := core.UserTree().Ancestors(db, userID)
	if errors.Is(err, hierarchy.ErrNotFound) {
		return 0, core.ErrNotFound.WithMessage(t(ctx, "user not found", "用户不存在"))
	}
	if err != nil {
		return 0, err
	}
	var from int64
	if len(down) > 0 {
		from = down[len(down)-1]
	}
	if !create && (owner == nil || *owner == from) {
		return from, nil
	}
	key := perm.UserCreate
	if !create {
		key = perm.UserUpdate
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
	case team:
		to = actorID // a create's default
	}
	if to == from {
		return to, nil
	}
	if !create && userID != actorID {
		if err := s.canActOnUsers(ctx, tx, actorID, core.ActOnIdentity, userID); err != nil {
			return 0, err
		}
	}
	up, err := s.moveUser(ctx, db, userID, to)
	if err != nil {
		return 0, err
	}
	// The user leaves the team of every user of the old chain and joins
	// that of every user of the new one: the actor must manage them all,
	// but for itself and those above it, whose teams hold the user either
	// way.
	var targets []int64
	for _, c := range [][]int64{down, up} {
		if i := slices.Index(c, actorID); i >= 0 {
			c = c[i+1:]
		}
		targets = append(targets, c...)
	}
	targets = slices.DeleteFunc(targets, func(id int64) bool { return id == actorID })
	if err := s.canActOnUsers(ctx, tx, actorID, core.ActOnIdentity, targets...); err != nil {
		return 0, err
	}
	return to, s.audit(ctx, tx, actorID, "user.owner", "user", userID, map[string]any{"from": idOrNil(from), "to": idOrNil(to)})
}

// moveUser moves userID under owner (0: none) in core.UserTree and
// returns its new chain of owners, top first. It fails, with a 400 on
// owner_id, unless owner is another live user, not below userID, its
// chain stays within core.MaxTeamDepth levels with userID's users below
// it, and it manages userID by its roles.
func (s *Service) moveUser(ctx context.Context, db *gorm.DB, userID, owner int64) ([]int64, error) {
	if owner != 0 {
		live, err := gormpg.Exists(db, db.Model(&model.User{}).Where("id = ? AND deleted_at IS NULL", owner))
		if err != nil {
			return nil, err
		}
		if !live {
			return nil, badOwner(ctx, "owner_id", hierarchy.ErrNotFound.Code())
		}
	}
	up, err := core.UserTree().Move(db, userID, owner)
	var refused hierarchy.Error
	if errors.As(err, &refused) {
		return nil, badOwner(ctx, "owner_id", refused.Code())
	}
	if err != nil || owner == 0 {
		return up, err
	}
	target, roles, err := s.usersRoles(ctx, db, []int64{userID})
	if err != nil {
		return nil, err
	}
	return up, s.ownerManages(ctx, db, owner, target, roles)
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
		return badOwner(ctx, "owner_id", "owner_cannot_manage")
	}
	return nil
}

// ownersKeepManaging is ownerManages after a write to what role key grants
// or may assign, refused on field, the write's: every live user with a
// live owner, either of them holding key, must still be one its owner
// manages (CONTRACTS §103.3). Users compare by their roles, so each pair of
// role sets is judged once; details.user_id names a user of a pair that
// fails.
func (s *Service) ownersKeepManaging(ctx context.Context, q *gorm.DB, key, field string) error {
	var pairs []struct {
		Owner, Roles store.Strings
		UserID       int64
	}
	if err := ownedRoleSets(store.G(ctx, q), key).Scan(&pairs).Error; err != nil {
		return err
	}
	sets := map[string]core.PermissionSet{}
	setOf := func(keys []string) (core.PermissionSet, error) {
		k := strings.Join(keys, " ")
		set, ok := sets[k]
		if !ok {
			var err error
			if set, err = s.RolesPermissions(ctx, q, keys); err != nil {
				return set, err
			}
			sets[k] = set
		}
		return set, nil
	}
	for _, p := range pairs {
		owner, err := setOf(p.Owner)
		if err != nil {
			return err
		}
		target, err := setOf(p.Roles)
		if err != nil {
			return err
		}
		if manageDenial(ctx, owner, target, p.Roles) != nil {
			return badOwner(ctx, field, "owner_cannot_manage").WithDetails(map[string]any{"user_id": p.UserID})
		}
	}
	return nil
}

// ownedRoleSets selects the distinct pairs of the role keys (sorted) of a
// live user with a live owner, either of them holding role key, and of its
// owner, with the least id of the users of each pair.
func ownedRoleSets(db *gorm.DB, key string) *gorm.DB {
	holders := func() *gorm.DB { return casbinrule.MembersOf(db, db.Model(&model.Role{}).Where("key = ?", key)) }
	pairs := db.Table("users u").Joins("JOIN users o ON o.id = u.owner_id AND o.deleted_at IS NULL").
		Select(`ARRAY(`+casbinrule.RolesOfOwner+` ORDER BY 1) AS owner, ARRAY(`+casbinrule.RolesOfUser+` ORDER BY 1) AS roles, u.id`).
		Where("u.deleted_at IS NULL AND (u.id IN (?) OR o.id IN (?))", holders(), holders())
	return db.Table("(?) AS p", pairs).Select("p.owner, p.roles, min(p.id) AS user_id").Group("p.owner, p.roles").Order("user_id")
}

// badOwner is the 400 of an owner refused for code, on field (owner_id,
// or the role write's for owner_cannot_manage).
func badOwner(ctx context.Context, field, code string) *core.Error {
	msg := map[string][2]string{
		"self":                {"a user cannot be its own owner", "用户不能是自己的 owner"},
		"not_found":           {"owner_id must be a user that is not deleted", "owner_id 必须是未删除的用户"},
		"cycle":               {"the owner is below this user", "owner 在该用户的下级之中"},
		"too_deep":            {"the owner chain would be more than 8 levels deep", "owner 链会超过 8 层"},
		"owner_cannot_manage": {"the owner cannot manage this user by its roles; change the owner first", "owner 按角色管理不了该用户，请先修改 owner"},
	}[code]
	return core.InvalidFields(core.FieldError{Field: field, Code: code, Message: t(ctx, msg[0], msg[1])})
}

// idOrNil is id for an audit detail: nil for 0 (none).
func idOrNil(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// ownerOf is the owner of uid, 0 for none.
func ownerOf(db *gorm.DB, uid int64) (int64, error) {
	var owners []int64
	if err := db.Model(&model.User{}).Where("id = ? AND owner_id IS NOT NULL", uid).Pluck("owner_id", &owners).Error; err != nil || len(owners) == 0 {
		return 0, err
	}
	return owners[0], nil
}

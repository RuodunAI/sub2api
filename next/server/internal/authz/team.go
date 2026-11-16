package authz

import (
	"context"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// userActions are the actions on users a team scope applies to
// (perm.ResourceUser).
var userActions = []perm.Key{perm.UserRead, perm.UserCreate, perm.UserUpdate}

// withinTeam is the team step of canActOnUsers (plan 1.8, 7.9): an actor
// (actor: its roles' set) reaching users by a team alone, holding
// user:read, user:create or user:update and none of them for every user,
// acts only on the users below it (core.UserTree), never on itself. ids
// are distinct; they are read through q, the change's transaction when
// there is one (C-L4). An actor holding none of them is not limited here.
func withinTeam(ctx context.Context, q *gorm.DB, actorID int64, actor core.PermissionSet, ids []int64) error {
	var reach core.DataScope
	for _, a := range userActions {
		reach = reach.Union(actor.DataScope(actorID, a))
	}
	if reach.All || reach.Empty() {
		return nil
	}
	db := store.G(ctx, q)
	var below int64
	err := db.Model(&model.User{}).Where("id = ANY(?) AND id IN (?)", store.Int64s(ids), core.UserTree().Below(db, actorID)).Count(&below).Error
	if err != nil {
		return err
	}
	if below < int64(len(ids)) {
		return core.ErrPermissionDenied.WithMessage(t(ctx,
			"you can only manage the users below you", "你只能管理自己的下级用户")).WithDetails(map[string]any{"reason": "not_in_team"})
	}
	return nil
}

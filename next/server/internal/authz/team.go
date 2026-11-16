package authz

import (
	"context"
	"fmt"
	"slices"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Teams (CONTRACTS §103.2): the users below a holder along users.owner_id,
// core.MaxTeamDepth levels at most, soft-deleted users included (D7). Read
// level by level through the query builder, one indexed query a level
// (users_owner_idx), never a recursive CTE (plan 7.3).

// teamCacheSize bounds the teams Team keeps, of the order of the users the
// enforcer holds (casbinx.Options.Users); a full cache drops a random one.
const teamCacheSize = 50_000

// cachedTeam is a team as Team read it, at authz version version.
type cachedTeam struct {
	version int64
	ids     []int64
}

// Team implements core.TeamResolver: the users below uid, read at an authz
// version of atLeast or newer. A team read at an older version is never
// answered: a write that moves an owner bumps the version naming the
// moved user (OwnerGuard), and invalidate drops the teams read before it.
// The ids are shared with the cache: callers must not change them. A team
// that cannot be read is unavailable (503), never empty.
func (s *Service) Team(ctx context.Context, uid, atLeast int64) ([]int64, error) {
	s.mu.RLock()
	cached, ok := s.teams[uid]
	s.mu.RUnlock()
	if ok && cached.version >= atLeast {
		return cached.ids, nil
	}
	if s.db == nil {
		return nil, errNoEnforcer
	}
	var c cachedTeam
	err := s.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		db := store.G(ctx, tx)
		var err error
		if c.version, _, err = store.AuthzVersions(db); err == nil {
			c.ids, _, err = subtree(db, uid)
		}
		return err
	})
	if err == nil && c.version < atLeast {
		err = fmt.Errorf("authz: team read at version %d, %d needed", c.version, atLeast)
	}
	if err != nil {
		return nil, unavailable(err)
	}
	s.mu.Lock()
	if c.version >= s.minVersion {
		if len(s.teams) >= teamCacheSize {
			for k := range s.teams {
				delete(s.teams, k)
				break
			}
		}
		s.teams[uid] = c
	}
	s.mu.Unlock()
	return c.ids, nil
}

// subtree returns the users below root, core.MaxTeamDepth levels at most,
// sorted, and how many levels they span (0: none).
func subtree(db *gorm.DB, root int64) ([]int64, int, error) {
	seen := map[int64]bool{root: true}
	var below []int64
	level := []int64{root}
	levels := 0
	for ; levels < core.MaxTeamDepth; levels++ {
		var next []int64
		if err := db.Model(&model.User{}).Where("owner_id = ANY(?)", store.Int64s(level)).Pluck("id", &next).Error; err != nil {
			return nil, 0, err
		}
		next = slices.DeleteFunc(next, func(id int64) bool { return seen[id] })
		if len(next) == 0 {
			break
		}
		for _, id := range next {
			seen[id] = true
		}
		below, level = append(below, next...), next
	}
	slices.Sort(below)
	return below, levels, nil
}

// ownerChain is uid and the owners above it, nearest first: none for 0, at
// most core.MaxTeamDepth+1 owners (a chain longer than any allowed).
func ownerChain(db *gorm.DB, uid int64) ([]int64, error) {
	var out []int64
	for cur := uid; cur != 0 && len(out) <= core.MaxTeamDepth+1; {
		out = append(out, cur)
		var err error
		if cur, err = ownerOf(db, cur); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ownerOf is the owner of uid, 0 for none.
func ownerOf(db *gorm.DB, uid int64) (int64, error) {
	var owners []int64
	if err := db.Model(&model.User{}).Where("id = ? AND owner_id IS NOT NULL", uid).Pluck("owner_id", &owners).Error; err != nil || len(owners) == 0 {
		return 0, err
	}
	return owners[0], nil
}

// userActions are the actions on users a team scope applies to
// (perm.ResourceUser).
var userActions = []perm.Key{perm.UserRead, perm.UserCreate, perm.UserUpdate}

// withinTeam is the team step of canActOnUsers (plan 1.8, 7.9): an actor
// (actor: its roles' set) reaching users by a team alone, holding
// user:read, user:create or user:update and none of them for every user,
// acts only on the users below it, never on itself. Its team is read
// through q, the change's transaction when there is one (C-L4), not
// Team's cache. An actor holding none of them is not limited here.
func withinTeam(ctx context.Context, q *gorm.DB, actorID int64, actor core.PermissionSet, ids []int64) error {
	var reach core.DataScope
	for _, a := range userActions {
		reach = reach.Union(actor.DataScope(actorID, a))
	}
	if reach.All || reach.Empty() {
		return nil
	}
	team, _, err := subtree(store.G(ctx, q), actorID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := slices.BinarySearch(team, id); !ok {
			return core.ErrPermissionDenied.WithMessage(t(ctx,
				"you can only manage the users below you", "你只能管理自己的下级用户")).WithDetails(map[string]any{"reason": "not_in_team"})
		}
	}
	return nil
}

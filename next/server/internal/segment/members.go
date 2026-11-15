package segment

import (
	"context"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// addMembers moves users into a named segment and answers how many moved.
func (s *Service) addMembers(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	var in struct {
		UserIDs []int64 `json:"user_ids"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if err := s.actOnUsers(ctx, in.UserIDs...); err != nil {
		httpapi.Fail(c, err)
		return
	}
	var moved int
	var dropped bool
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// The lock keeps the segment from being deleted under the move.
		kind, _, err := segmentKind(ctx, tx, id, "KEY SHARE")
		if err != nil {
			return err
		}
		if kind == core.SegmentPersonal {
			return core.ErrConflict.WithMessage(t(ctx, "a personal user group has one member", "个人专属只有一个成员"))
		}
		if err := requireUsers(ctx, tx, in.UserIDs); err != nil {
			return err
		}
		moved, dropped, err = moveUsers(ctx, tx, id, in.UserIDs, "")
		return err
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if dropped {
		s.priceRulesChanged(ctx)
	}
	httpapi.OK(c, gin.H{"moved": moved})
}

// requireUsers fails with the field error of the first id that is not a
// user; a deleted user is none.
func requireUsers(ctx context.Context, q store.Querier, ids []int64) error {
	var unknown int64
	err := store.G(ctx, q).Raw(`SELECT i.id FROM unnest(?::bigint[]) WITH ORDINALITY AS i(id, n)
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = i.id AND u.deleted_at IS NULL)
		ORDER BY i.n LIMIT 1`, store.Int64s(ids)).Take(&unknown).Error
	if store.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	id := strconv.FormatInt(unknown, 10)
	return core.InvalidFields(fieldError(ctx, "user_ids", "not_found", "unknown user "+id, "用户 "+id+" 不存在"))
}

// moveUsers puts the users into the named segment to and deletes the
// personal segment of each one who had one, with its settings. With from a
// segment kind, only users in a segment of that kind move. It returns how
// many moved (a user already there does not) and whether a personal segment
// went: its price rules went with it.
func moveUsers(ctx context.Context, tx pgx.Tx, to int64, userIDs []int64, from string) (moved int, dropped bool, err error) {
	ids, err := lockUsers(ctx, tx, movingUsers, store.Int64s(userIDs), to, from, from)
	if err != nil {
		return 0, false, err
	}
	if err := store.G(ctx, tx).Model(&model.User{}).Where("id = ANY(?)", store.Int64s(ids)).Update("segment_id", to).Error; err != nil {
		return 0, false, err
	}
	dropped, err = DropPersonal(ctx, tx, ids...)
	return len(ids), dropped, err
}

// movingUsers picks the users of ids (the first parameter) that are not
// deleted and not in the segment to (the second), only those in a segment of
// the kind from (the last two, "" = any).
const movingUsers = `id = ANY(?) AND deleted_at IS NULL AND segment_id <> ?
	AND (? = '' OR segment_id IN (SELECT id FROM segments WHERE kind = ?))`

// lockUsers locks the users cond picks, in id order, until the transaction
// ends, and returns their ids. Every writer of users.segment_id locks first
// and updates in a statement of its own: an UPDATE in the statement that
// locks starts from the row versions of that statement's snapshot and, where
// another writer replaced one meanwhile, queues on it behind the key-share
// holders (inserts that refer to the user), which can close a cycle.
func lockUsers(ctx context.Context, tx pgx.Tx, cond string, args ...any) ([]int64, error) {
	var ids []int64
	err := lockUsersQuery(store.G(ctx, tx), cond, args...).Pluck("id", &ids).Error
	return ids, err
}

// lockUsersQuery selects the users cond picks, locked in id order (lockUsers).
func lockUsersQuery(db *gorm.DB, cond string, args ...any) *gorm.DB {
	return db.Model(&model.User{}).Where(cond, args...).Order("id").Clauses(clause.Locking{Strength: "NO KEY UPDATE"})
}

// DropPersonal deletes the personal segments of the users, who have left
// them, with their grants, rates and model prices. It reports whether one
// went: the cached price rules are then stale, and the caller drops them
// after the transaction (Deps.PriceRulesChanged).
func DropPersonal(ctx context.Context, tx pgx.Tx, userIDs ...int64) (bool, error) {
	res := store.G(ctx, tx).Where("kind = 'personal' AND user_id = ANY(?)", store.Int64s(userIDs)).Delete(&model.Segment{})
	return res.RowsAffected > 0, res.Error
}

package providerresources

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func queryFilter(owner core.ResourceOwner, q core.ResourceQuery) (string, []any, error) {
	if !validOwner(owner) || !bounded(q.PluginKey) || !bounded(q.Kind) || q.Limit < 1 || q.Limit > 1000 || len(q.IDs) > 100 || (q.BeforeID != "" && q.AfterID != "") {
		return "", nil, core.ErrInvalidArgument
	}
	for _, id := range append(slices.Clone(q.IDs), q.BeforeID, q.AfterID) {
		if id != "" && !bounded(id) {
			return "", nil, core.ErrInvalidArgument
		}
	}
	for _, id := range q.AccountIDs {
		if id <= 0 {
			return "", nil, core.ErrInvalidArgument
		}
	}
	args := []any{owner.UserID, owner.GroupID, q.PluginKey, q.Kind, store.Int64s(q.AccountIDs)}
	where := `user_id=? AND group_id=? AND plugin_key=? AND kind=? AND account_id=ANY(?::bigint[])`
	if q.ExpiredWithin < 0 || q.ExpiredWithin > 365*24*time.Hour || (q.UnexpiredOnly && q.ExpiredWithin > 0) {
		return "", nil, core.ErrInvalidArgument
	}
	if q.ReadyOnly {
		where += ` AND state='ready'`
	}
	if q.UnexpiredOnly {
		where += ` AND (expires_at IS NULL OR expires_at>now())`
	}
	if q.ExpiredWithin > 0 {
		args = append(args, q.ExpiredWithin.Seconds())
		where += ` AND (expires_at IS NULL OR expires_at>now()-(? * interval '1 second'))`
	}
	if len(q.IDs) > 0 {
		args = append(args, store.Strings(q.IDs))
		where += ` AND public_id=ANY(?::text[])`
	}
	return where, args, nil
}

// Query filters inside SQL before pagination. A read-only repeatable snapshot
// makes cursor eligibility and page membership consistent during concurrent
// completion/deletion. Public IDs only break ties in provider creation order.
func (s *Service) Query(ctx context.Context, owner core.ResourceOwner, q core.ResourceQuery) (core.ResourcePage, error) {
	return s.query(ctx, owner, q, false)
}
func (s *Service) query(ctx context.Context, owner core.ResourceOwner, q core.ResourceQuery, skillVersions bool) (out core.ResourcePage, err error) {
	where, args, err := queryFilter(owner, q)
	if err != nil {
		return out, err
	}
	if skillVersions {
		where += ` AND EXISTS(SELECT 1 FROM provider_skill_versions v WHERE v.parent_id=provider_resources.public_id AND v.state='ready')`
	}
	out.Items = []core.ProviderResource{}
	if len(q.AccountIDs) == 0 {
		return out, nil
	}
	tx, err := s.db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	g := store.G(ctx, tx)
	cursor := q.AfterID
	if q.BeforeID != "" {
		cursor = q.BeforeID
	}
	if cursor != "" {
		r, _, e := read(g, where+` AND public_id=?`, append(slices.Clone(args), cursor)...)
		if store.IsNoRows(e) {
			return out, core.ErrNotFound
		}
		if e != nil {
			return out, e
		}
		op := "<"
		if q.BeforeID != "" {
			op = ">"
		}
		args = append(args, r.CreatedAt, r.PublicID)
		where += ` AND (created_at,public_id)` + op + `(?,?)`
	}
	direction := "DESC"
	if q.BeforeID != "" {
		direction = "ASC"
	}
	var rows []model.ProviderResource
	if err = page(g, where, args, direction, q.Limit+1).Find(&rows).Error; err != nil {
		return out, err
	}
	out.Items = resourceList(rows)
	out.HasMore = len(out.Items) > q.Limit
	if out.HasMore {
		out.Items = out.Items[:q.Limit]
	}
	if strings.EqualFold(direction, "ASC") {
		slices.Reverse(out.Items)
	}
	err = tx.Commit(ctx)
	return out, err
}

// page selects up to limit resources that where (with args) keeps, in
// creation order by direction (ASC or DESC), public IDs breaking ties.
func page(db *gorm.DB, where string, args []any, direction string, limit int) *gorm.DB {
	return resources(db).Where(where, args...).Order("created_at " + direction + ",public_id " + direction).Limit(limit)
}

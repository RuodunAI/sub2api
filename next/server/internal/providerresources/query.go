package providerresources

import (
	"context"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func queryFilter(owner core.ResourceOwner, q core.ResourceQuery) (store.Conds, error) {
	if !validOwner(owner) || !bounded(q.PluginKey) || !bounded(q.Kind) || q.Limit < 1 || q.Limit > 1000 || len(q.IDs) > 100 || (q.BeforeID != "" && q.AfterID != "") {
		return nil, core.ErrInvalidArgument
	}
	for _, id := range append(slices.Clone(q.IDs), q.BeforeID, q.AfterID) {
		if id != "" && !bounded(id) {
			return nil, core.ErrInvalidArgument
		}
	}
	for _, id := range q.AccountIDs {
		if id <= 0 {
			return nil, core.ErrInvalidArgument
		}
	}
	if q.ExpiredWithin < 0 || q.ExpiredWithin > 365*24*time.Hour || (q.UnexpiredOnly && q.ExpiredWithin > 0) {
		return nil, core.ErrInvalidArgument
	}
	var conds store.Conds
	conds.And(`user_id=? AND group_id=? AND plugin_key=? AND kind=? AND account_id=ANY(?::bigint[])`,
		owner.UserID, owner.GroupID, q.PluginKey, q.Kind, store.Int64s(q.AccountIDs))
	if q.ReadyOnly {
		conds.And(`state='ready'`)
	}
	if q.UnexpiredOnly {
		conds.And(`(expires_at IS NULL OR expires_at>now())`)
	}
	if q.ExpiredWithin > 0 {
		conds.And(`(expires_at IS NULL OR expires_at>now()-(? * interval '1 second'))`, q.ExpiredWithin.Seconds())
	}
	if len(q.IDs) > 0 {
		conds.And(`public_id=ANY(?::text[])`, store.Strings(q.IDs))
	}
	return conds, nil
}

// Query filters inside SQL before pagination. A read-only repeatable snapshot
// makes cursor eligibility and page membership consistent during concurrent
// completion/deletion. Public IDs only break ties in provider creation order.
func (s *Service) Query(ctx context.Context, owner core.ResourceOwner, q core.ResourceQuery) (core.ResourcePage, error) {
	return s.query(ctx, owner, q, false)
}
func (s *Service) query(ctx context.Context, owner core.ResourceOwner, q core.ResourceQuery, skillVersions bool) (out core.ResourcePage, err error) {
	conds, err := queryFilter(owner, q)
	if err != nil {
		return out, err
	}
	if skillVersions {
		conds.And(`EXISTS(SELECT 1 FROM provider_skill_versions v WHERE v.parent_id=provider_resources.public_id AND v.state='ready')`)
	}
	out.Items = []core.ProviderResource{}
	if len(q.AccountIDs) == 0 {
		return out, nil
	}
	backwards := q.BeforeID != "" // ascending, from the cursor
	var rows []model.ProviderResource
	err = s.db.TxOptions(ctx, store.Snapshot(), func(tx *gorm.DB) error {
		g := store.G(ctx, tx)
		cursor := q.AfterID
		if q.BeforeID != "" {
			cursor = q.BeforeID
		}
		if cursor != "" {
			r, _, e := read(g.Scopes(conds.Scope()), `public_id=?`, cursor)
			if store.IsNoRows(e) {
				return core.ErrNotFound
			}
			if e != nil {
				return e
			}
			after := `(created_at,public_id)<(?,?)`
			if backwards {
				after = `(created_at,public_id)>(?,?)`
			}
			conds.And(after, r.CreatedAt, r.PublicID)
		}
		return page(g, conds, backwards, q.Limit+1).Find(&rows).Error
	})
	if err != nil {
		return out, err
	}
	out.Items = resourceList(rows)
	out.HasMore = len(out.Items) > q.Limit
	if out.HasMore {
		out.Items = out.Items[:q.Limit]
	}
	if backwards {
		slices.Reverse(out.Items)
	}
	return out, nil
}

// page selects up to limit resources that conds keep, newest first (oldest
// first when asc), public IDs breaking ties.
func page(db *gorm.DB, conds store.Conds, asc bool, limit int) *gorm.DB {
	return resources(db).Scopes(conds.Scope()).Order(clause.OrderBy{Columns: []clause.OrderByColumn{
		{Column: clause.Column{Name: "created_at"}, Desc: !asc}, {Column: clause.Column{Name: "public_id"}, Desc: !asc}}}).Limit(limit)
}

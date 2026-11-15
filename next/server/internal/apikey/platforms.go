package apikey

import (
	"context"
	"sort"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The group module computes the same view for GET /groups; modules do not
// import each other (CONTRACTS §1), so the small helper is duplicated.

// groupTypes returns the distinct account types (plugin key + type id) of the
// live accounts in each of the groups, in one query.
func groupTypes(ctx context.Context, q store.Querier, ids []int64) (map[int64][]core.AccountTypeKey, error) {
	out := map[int64][]core.AccountTypeKey{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []groupTypeRow
	if err := groupTypesQuery(store.G(ctx, q), ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.GroupID] = append(out[r.GroupID], core.AccountTypeKey{PluginKey: r.PluginKey, Type: r.Type})
	}
	return out, nil
}

// groupTypeRow is a row of groupTypesQuery.
type groupTypeRow struct {
	GroupID         int64
	PluginKey, Type string
}

// groupTypesQuery selects the distinct account types of the live accounts in
// the groups ids.
func groupTypesQuery(db *gorm.DB, ids []int64) *gorm.DB {
	return db.Table("account_groups ag").Distinct("ag.group_id, a.plugin_key, a.type").
		Joins("JOIN accounts a ON a.id = ag.account_id").
		Where("ag.group_id = ANY(?) AND a.deleted_at IS NULL", store.Int64s(ids))
}

// platformsOf returns the sorted, distinct platforms of the generation that
// the account types declare; unregistered types serve none.
func platformsOf(gen core.Generation, types []core.AccountTypeKey) []string {
	out := []string{}
	if gen == nil {
		return out
	}
	seen := map[string]bool{}
	for _, k := range types {
		b, ok := gen.AccountType(k.PluginKey, k.Type)
		if !ok {
			continue
		}
		for _, ap := range b.Type.Platforms {
			if seen[ap.Platform] {
				continue
			}
			if _, ok := gen.Platform(ap.Platform); ok {
				seen[ap.Platform] = true
				out = append(out, ap.Platform)
			}
		}
	}
	sort.Strings(out)
	return out
}

// fillPlatforms sets Platforms of the keys to the platforms of their groups
// (CONTRACTS §13), with one query. Without a registry or generation the
// lists are empty.
func (s *Service) fillPlatforms(ctx context.Context, keys []*APIKey) error {
	var gen core.Generation
	if s.reg != nil {
		gen = s.reg.Current()
	}
	byGroup := map[int64][]string{}
	if gen != nil && len(keys) > 0 {
		ids := make([]int64, 0, len(keys))
		seen := map[int64]bool{}
		for _, k := range keys {
			if k.GroupID != nil && !seen[*k.GroupID] {
				seen[*k.GroupID] = true
				ids = append(ids, *k.GroupID)
			}
		}
		types, err := groupTypes(ctx, s.db.Pool, ids)
		if err != nil {
			return err
		}
		for _, id := range ids {
			byGroup[id] = platformsOf(gen, types[id])
		}
	}
	for _, k := range keys {
		if k.GroupID == nil {
			k.Platforms = []string{}
		} else if ps, ok := byGroup[*k.GroupID]; ok {
			k.Platforms = ps
		} else {
			k.Platforms = []string{}
		}
	}
	return nil
}

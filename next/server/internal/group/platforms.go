package group

import (
	"context"
	"sort"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// groupTypes returns the distinct account types (plugin key + type id) of the
// live accounts in each of the groups, in one query.
func groupTypes(ctx context.Context, q *gorm.DB, ids []int64) (map[int64][]core.AccountTypeKey, error) {
	out := map[int64][]core.AccountTypeKey{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		GroupID int64 `gorm:"column:group_id"`
		core.AccountTypeKey
	}
	if err := liveAccounts(store.G(ctx, q), ids).Distinct("ag.group_id", "a.plugin_key", "a.type").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.GroupID] = append(out[r.GroupID], r.AccountTypeKey)
	}
	return out, nil
}

// liveAccounts joins the groups of ids (account_groups ag) to their live
// accounts (a).
func liveAccounts(db *gorm.DB, ids []int64) *gorm.DB {
	return db.Table("account_groups ag").Joins("JOIN accounts a ON a.id = ag.account_id").
		Where("ag.group_id = ANY(?) AND a.deleted_at IS NULL", store.Int64s(ids))
}

// platformResolver maps account types to the platforms they serve in one
// generation, memoizing per type.
type platformResolver struct {
	gen  core.Generation
	memo map[core.AccountTypeKey][]string
}

func newPlatformResolver(gen core.Generation) *platformResolver {
	return &platformResolver{gen: gen, memo: map[core.AccountTypeKey][]string{}}
}

// typePlatforms returns the platforms the account type declares that exist
// in the generation; none when the type is not registered.
func (r *platformResolver) typePlatforms(k core.AccountTypeKey) []string {
	if ps, ok := r.memo[k]; ok {
		return ps
	}
	var ps []string
	if b, ok := r.gen.AccountType(k.PluginKey, k.Type); ok {
		for _, ap := range b.Type.Platforms {
			if _, ok := r.gen.Platform(ap.Platform); ok {
				ps = append(ps, ap.Platform)
			}
		}
	}
	r.memo[k] = ps
	return ps
}

// platforms returns the sorted, distinct platforms served by the types.
func (r *platformResolver) platforms(types []core.AccountTypeKey) []string {
	out := []string{}
	if r == nil || r.gen == nil {
		return out
	}
	seen := map[string]bool{}
	for _, k := range types {
		for _, p := range r.typePlatforms(k) {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Platforms returns, for each group, the platforms its API keys can
// reach: those served by the account types of its live accounts (CONTRACTS
// §13). Every requested id gets a non-nil slice; without a registry or
// generation the slices are empty.
func (s *Service) Platforms(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	var gen core.Generation
	if s.reg != nil {
		gen = s.reg.Current()
	}
	if gen == nil {
		for _, id := range ids {
			out[id] = []string{}
		}
		return out, nil
	}
	types, err := groupTypes(ctx, s.db.Gorm, ids)
	if err != nil {
		return nil, err
	}
	r := newPlatformResolver(gen)
	for _, id := range ids {
		out[id] = r.platforms(types[id])
	}
	return out, nil
}

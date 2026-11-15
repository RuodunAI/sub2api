package group

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Models contains only request model IDs; it never exposes account credentials.
type Models struct {
	Models               []string `json:"models"`
	UnrestrictedAccounts int      `json:"unrestricted_accounts"`
}

func (s *Service) models(c *gin.Context) {
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if _, err := s.loadRow(ctx, id); err != nil {
		httpapi.Fail(c, err)
		return
	}
	byGroup, err := ProvidedModels(ctx, s.db.Pool, []int64{id})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, byGroup[id])
}

// ProvidedModels returns, for each of the groups, the request models its
// accounts declare, sorted, and how many of its accounts declare none (they
// serve every model). Deleted accounts do not count. Every asked group has an
// entry.
func ProvidedModels(ctx context.Context, q store.Querier, groupIDs []int64) (map[int64]Models, error) {
	var rows []struct {
		GroupID int64         `gorm:"column:group_id"`
		Models  store.Strings `gorm:"column:models"`
	}
	if err := liveAccounts(store.G(ctx, q), groupIDs).Select("ag.group_id, a.models").Scan(&rows).Error; err != nil {
		return nil, err
	}
	sets := make(map[int64]map[string]bool, len(groupIDs))
	out := make(map[int64]Models, len(groupIDs))
	for _, id := range groupIDs {
		sets[id] = map[string]bool{}
		out[id] = Models{Models: []string{}}
	}
	for _, r := range rows {
		m := out[r.GroupID]
		if len(r.Models) == 0 {
			m.UnrestrictedAccounts++
		}
		for _, model := range r.Models {
			model = strings.TrimSpace(model)
			if model != "" && !sets[r.GroupID][model] {
				sets[r.GroupID][model] = true
				m.Models = append(m.Models, model)
			}
		}
		out[r.GroupID] = m
	}
	for _, m := range out {
		sort.Strings(m.Models)
	}
	return out, nil
}

// Offers reports whether a group with these accounts provides model: an
// account declares it, or one declares none and the model has an enabled
// global price (priced), and the model policy of the group allows it. It is
// the one place the rule is written. Models must be sorted, as
// ProvidedModels returns them.
func (m Models) Offers(model string, priced bool, policy core.GroupInfo) bool {
	_, declared := slices.BinarySearch(m.Models, model)
	return (declared || priced && m.UnrestrictedAccounts > 0) && policy.AllowsModel(model)
}

// Offered returns the models a group with these accounts provides (Offers),
// sorted; priced are the models with an enabled global price.
func (m Models) Offered(priced []string, policy core.GroupInfo) []string {
	out := []string{}
	keep := func(names []string, havePrice bool) {
		for _, name := range names {
			if m.Offers(name, havePrice, policy) {
				out = append(out, name)
			}
		}
	}
	keep(m.Models, false)
	keep(priced, true)
	slices.Sort(out)
	return slices.Compact(out)
}

// OfferedModels returns the models each of the groups provides
// (Models.Offered), by group id; priced are the models with an enabled global
// price. Of a group it reads the id and the model policy.
func OfferedModels(ctx context.Context, q store.Querier, groups []core.GroupInfo, priced []string) (map[int64][]string, error) {
	provided, err := providedModels(ctx, q, groups)
	if err != nil {
		return nil, err
	}
	out := make(map[int64][]string, len(groups))
	for _, g := range groups {
		out[g.ID] = provided[g.ID].Offered(priced, g)
	}
	return out, nil
}

// providedModels returns ProvidedModels of the groups.
func providedModels(ctx context.Context, q store.Querier, groups []core.GroupInfo) (map[int64]Models, error) {
	ids := make([]int64, len(groups))
	for i, g := range groups {
		ids[i] = g.ID
	}
	return ProvidedModels(ctx, q, ids)
}

// offeredModels returns OfferedModels of the groups against the models that
// have an enabled global price now.
func offeredModels(ctx context.Context, q store.Querier, groups []core.GroupInfo) (map[int64][]string, error) {
	var priced []string
	if err := store.G(ctx, q).Model(&model.ModelPrice{}).Where("enabled").Pluck("model", &priced).Error; err != nil {
		return nil, err
	}
	return OfferedModels(ctx, q, groups, priced)
}

// groupsProviding returns the ids of the groups that provide the model name
// (Models.Offers); never nil.
func (s *Service) groupsProviding(ctx context.Context, name string) ([]int64, error) {
	db := store.G(ctx, s.db.Pool)
	var rows []model.Group
	if err := db.Select("id", "model_allowlist", "model_filter_mode").Find(&rows).Error; err != nil {
		return nil, err
	}
	groups := make([]core.GroupInfo, len(rows))
	for i, r := range rows {
		allow, err := allowlist(r.ModelAllowlist)
		if err != nil {
			return nil, err
		}
		groups[i] = core.GroupInfo{ID: r.ID, ModelAllowlist: allow, ModelFilterMode: r.ModelFilterMode}
	}
	provided, err := providedModels(ctx, s.db.Pool, groups)
	if err != nil {
		return nil, err
	}
	var priced bool
	if err := db.Raw("SELECT EXISTS (?)", db.Model(&model.ModelPrice{}).Select("1").Where("model = ? AND enabled", name)).
		Scan(&priced).Error; err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, g := range groups {
		if provided[g.ID].Offers(name, priced, g) {
			ids = append(ids, g.ID)
		}
	}
	return ids, nil
}

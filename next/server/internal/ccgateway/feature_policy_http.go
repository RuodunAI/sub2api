package ccgateway

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strconv"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

type featureGroupRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type featurePreview struct {
	FeatureResolution
	AccountPolicy AccountFeaturePolicy `json:"cc_feature_policy"`
	Groups        []featureGroupRef    `json:"groups"`
}

func (s *Service) accountFeaturePreview(c *gin.Context) {
	httpapi.NoStore(c)
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if !s.previewAuthorize(c, ctx, id) {
		return
	}
	groupID, draft, ok := previewInput(c)
	if !ok {
		return
	}
	groups, ok := s.accountGroups(c, ctx, id, groupID)
	if !ok {
		return
	}
	account, err := LoadAccountFeaturePolicy(ctx, s.DB.Pool, id)
	if err != nil {
		httpapi.Fail(c, core.ErrUnavailable)
		return
	}
	cfg, err := s.Load(ctx)
	if err != nil {
		httpapi.Fail(c, core.ErrUnavailable)
		return
	}
	if draft != nil {
		if draft.Revision != account.Revision {
			httpapi.Fail(c, featureRevisionConflict())
			return
		}
		account = *draft
	}
	out := featurePreview{FeatureResolution: cfg.ResolveFeaturePolicy(groupID, account), AccountPolicy: account, Groups: groups}
	httpapi.OK(c, out)
}

// previewAuthorize confirms the caller may act on the account for this
// method and that it is a live CCGateway account.
//
// An own-level caller sees the whole resolution, inherited values and their
// sources (group and global policy) included (won't fix): they decide how its
// own account behaves and name no other account.
func (s *Service) previewAuthorize(c *gin.Context, ctx context.Context, id int64) bool {
	if err := s.authorizeAccount(ctx, c.Request.Method, id); err != nil {
		httpapi.Fail(c, err)
		return false
	}
	g := store.G(ctx, s.DB.Pool)
	found, err := rowExists(g, g.Model(&model.Account{}).
		Where("id = ? AND deleted_at IS NULL AND plugin_key = 'ccgateway' AND type IN ('managed', 'apikey')", id))
	if err != nil || !found {
		httpapi.Fail(c, core.ErrNotFound)
		return false
	}
	return true
}

// previewInput reads the optional group and draft policy from a POST body or
// the GET query.
func previewInput(c *gin.Context) (groupID int64, draft *AccountFeaturePolicy, ok bool) {
	if c.Request.Method == "POST" {
		var in struct {
			GroupID int64                 `json:"group_id"`
			Policy  *AccountFeaturePolicy `json:"cc_feature_policy"`
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
		if !httpapi.BindJSON(c, &in) {
			return 0, nil, false
		}
		if in.Policy != nil {
			if err := in.Policy.Validate(); err != nil {
				httpapi.Fail(c, core.ErrInvalidArgument.WithMessage(err.Error()))
				return 0, nil, false
			}
		}
		if in.GroupID < 0 {
			httpapi.Fail(c, core.ErrInvalidArgument)
			return 0, nil, false
		}
		return in.GroupID, in.Policy, true
	}
	if raw := c.Query("group_id"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			httpapi.Fail(c, core.ErrInvalidArgument)
			return 0, nil, false
		}
		return v, nil, true
	}
	return 0, nil, true
}

// accountGroups lists the account's groups and confirms groupID is one of
// them (0 means "no group selected").
func (s *Service) accountGroups(c *gin.Context, ctx context.Context, id, groupID int64) ([]featureGroupRef, bool) {
	var groups []featureGroupRef
	if err := accountGroupsQuery(store.G(ctx, s.DB.Pool), id).Scan(&groups).Error; err != nil {
		httpapi.Fail(c, core.ErrUnavailable)
		return nil, false
	}
	if groups == nil {
		groups = []featureGroupRef{} // a JSON list
	}
	found := groupID == 0 || slices.ContainsFunc(groups, func(g featureGroupRef) bool { return g.ID == groupID })
	if !found {
		httpapi.Fail(c, core.ErrNotFound)
		return nil, false
	}
	return groups, true
}

// accountGroupsQuery lists the groups of account id.
func accountGroupsQuery(db *gorm.DB, id int64) *gorm.DB {
	return db.Table("groups g").Joins("JOIN account_groups a ON a.group_id = g.id").
		Select("g.id, g.name").Where("a.account_id = ?", id).Order("g.id")
}

// Saving a custom draft requires at least one current account group without an
// enabled prohibition. Groups need no separate CC configuration. Retaining an
// unchanged dormant draft is allowed when editing other fields.
func (s *Service) SaveAccountFeatures(ctx context.Context, tx pgx.Tx, id int64, p AccountFeaturePolicy) error {
	old, err := LoadAccountFeaturePolicy(ctx, tx, id)
	if err != nil {
		return err
	}
	if p.Mode == "custom" && (old.Mode != p.Mode || !reflect.DeepEqual(old.Overrides, p.Overrides)) {
		cfg, _, err := s.loadIn(ctx, tx, true)
		if err != nil {
			return err
		}
		var gids []int64
		if err := store.G(ctx, tx).Model(&model.AccountGroup{}).Where("account_id = ?", id).Pluck("group_id", &gids).Error; err != nil {
			return err
		}
		if !slices.ContainsFunc(gids, cfg.allowsAccountFeatures) {
			return core.ErrPermissionDenied.WithMessage("No account group permits CC feature overrides.")
		}
	}
	return SaveAccountFeaturePolicyTx(ctx, tx, id, p)
}

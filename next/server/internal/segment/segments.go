package segment

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Segment is the API view of a segments row.
type Segment struct {
	core.SegmentRef
	Description string `json:"description"`
	// Discount multiplies the default rate of every group; 1 = none.
	Discount    decimal.Decimal `json:"discount"`
	IsDefault   bool            `json:"is_default"`
	MemberCount int64           `json:"member_count"`
	// GrantedGroups are the private groups the segment is granted.
	GrantedGroups []core.GroupRef `json:"granted_groups"`
	// RateOverrideCount counts the groups whose rate it overrides and
	// PriceRuleCount its own model prices, over all groups, on the models
	// each group provides now (Deps.RuleCounts).
	RateOverrideCount int64 `json:"rate_override_count"`
	PriceRuleCount    int64 `json:"price_rule_count"`
	*Personal
	CreatedAt time.Time `json:"created_at"`
}

// Personal is what only a personal segment has.
type Personal struct {
	// User is the member; callers that may not read users do not get it.
	User *Member `json:"user,omitempty"`
	// Origin is the segment the user came from; null once it is deleted.
	Origin *core.SegmentRef `json:"origin"`
}

// Member is the user of a personal segment.
type Member struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// fromSegments reads the segments s with the users u of the personal ones.
func fromSegments(db *gorm.DB) *gorm.DB {
	return db.Table("segments s").Joins("LEFT JOIN users u ON u.id = s.user_id")
}

// segmentColumns are the columns of a segmentRow: a segment with its member
// count, grants and rates; its price_rule_count comes from countRules. A
// deleted user is no member.
const segmentColumns = RefColumns + `, s.description, s.discount, s.is_default, s.created_at,
	(SELECT count(*) FROM users m WHERE m.segment_id = s.id AND m.deleted_at IS NULL) AS member_count,
	(SELECT COALESCE(jsonb_agg(jsonb_build_object('id', g.id, 'name', g.name) ORDER BY g.id), '[]')
	  FROM segment_groups sg JOIN groups g ON g.id = sg.group_id
	  WHERE sg.segment_id = s.id AND sg.granted AND g.visibility = 'restricted') AS granted_groups,
	(SELECT count(*) FROM segment_groups sg WHERE sg.segment_id = s.id AND sg.rate_override IS NOT NULL) AS rate_override_count,
	COALESCE(u.display_name, '') AS display_name, o.id AS origin_id, o.kind AS origin_kind, o.name AS origin_name`

// selectSegments reads the segmentRows of the segments s the caller narrows.
func selectSegments(db *gorm.DB) *gorm.DB {
	return fromSegments(db).Select(segmentColumns).Joins("LEFT JOIN segments o ON o.id = s.origin_id")
}

// segmentRow is a row of segmentColumns.
type segmentRow struct {
	core.SegmentRef
	Description       string          `gorm:"column:description"`
	Discount          decimal.Decimal `gorm:"column:discount"`
	IsDefault         bool            `gorm:"column:is_default"`
	CreatedAt         time.Time       `gorm:"column:created_at"`
	MemberCount       int64           `gorm:"column:member_count"`
	GrantedGroups     json.RawMessage `gorm:"column:granted_groups"`
	RateOverrideCount int64           `gorm:"column:rate_override_count"`
	DisplayName       string          `gorm:"column:display_name"`
	OriginID          *int64          `gorm:"column:origin_id"`
	OriginKind        *string         `gorm:"column:origin_kind"`
	OriginName        *string         `gorm:"column:origin_name"`
}

// segment is the row as a caller may see it (Masked).
func (r *segmentRow) segment(showUsers bool) (*Segment, error) {
	sg := Segment{SegmentRef: r.SegmentRef, Description: r.Description, Discount: r.Discount, IsDefault: r.IsDefault,
		MemberCount: r.MemberCount, RateOverrideCount: r.RateOverrideCount, CreatedAt: r.CreatedAt}
	if err := json.Unmarshal(r.GrantedGroups, &sg.GrantedGroups); err != nil {
		return nil, err
	}
	if sg.UserID == nil {
		return &sg, nil
	}
	sg.Personal = &Personal{}
	if showUsers {
		sg.User = &Member{ID: *sg.UserID, Email: sg.Name, DisplayName: r.DisplayName}
	}
	if r.OriginID != nil {
		sg.Origin = &core.SegmentRef{ID: *r.OriginID, Kind: *r.OriginKind, Name: *r.OriginName}
	}
	sg.SegmentRef = Masked(sg.SegmentRef, showUsers)
	return &sg, nil
}

func (s *Service) load(ctx context.Context, id int64) (*Segment, error) {
	show, err := ShowUsers(ctx, s.authz)
	if err != nil {
		return nil, err
	}
	var row segmentRow
	err = selectSegments(store.G(ctx, s.db.Pool)).Where("s.id = ?", id).Take(&row).Error
	if store.IsNoRows(err) {
		return nil, NotFound(ctx)
	}
	if err != nil {
		return nil, err
	}
	sg, err := row.segment(show)
	if err != nil {
		return nil, err
	}
	return sg, s.countRules(ctx, sg)
}

// countRules sets the price rule counts of the segments (Deps.RuleCounts).
func (s *Service) countRules(ctx context.Context, segments ...*Segment) error {
	ids := make([]int64, len(segments))
	for i, sg := range segments {
		ids[i] = sg.ID
	}
	counts, err := s.ruleCounts(ctx, ids)
	if err != nil {
		return err
	}
	for _, sg := range segments {
		sg.PriceRuleCount = 0
		for _, n := range counts[sg.ID] {
			sg.PriceRuleCount += n
		}
	}
	return nil
}

// render answers with the segment as the caller may see it.
func (s *Service) render(c *gin.Context, id int64, respond func(*gin.Context, any)) {
	sg, err := s.load(c.Request.Context(), id)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	respond(c, sg)
}

// searchSegments narrows db to the segments matching q. Users are only
// searched for callers that may read them: a match would tell an email apart
// from the others.
func searchSegments(db *gorm.DB, q string, showUsers bool) *gorm.DB {
	pattern := "%" + store.Like(q) + "%"
	cond, args := `s.name ILIKE ? OR s.description ILIKE ?`, []any{pattern, pattern}
	if showUsers {
		cond, args = cond+` OR u.email ILIKE ? OR u.display_name ILIKE ?`, append(args, pattern, pattern)
	}
	return db.Where(`(`+cond+`)`, args...)
}

// listFilter narrows the segments s to those of the kind that match q ("" =
// any).
func listFilter(kind, q string, showUsers bool) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		db = db.Where("s.kind = ?", kind)
		if q != "" {
			db = searchSegments(db, q, showUsers)
		}
		return db
	}
}

// list returns a page of the named segments, the default one first, or with
// ?kind=personal of the personal ones.
func (s *Service) list(c *gin.Context) {
	ctx := c.Request.Context()
	show, err := ShowUsers(ctx, s.authz)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	kind := core.SegmentNamed
	if c.Query("kind") == core.SegmentPersonal {
		kind = core.SegmentPersonal
	}
	g, filter := store.G(ctx, s.db.Pool), listFilter(kind, strings.TrimSpace(c.Query("q")), show)
	var total int64
	if err := fromSegments(g).Scopes(filter).Count(&total).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	page, size := httpapi.Pagination(c)
	var rows []segmentRow
	if err := selectSegments(g).Scopes(filter).Order("s.is_default DESC, s.id").Limit(size).Offset((page - 1) * size).
		Scan(&rows).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	items := make([]*Segment, 0, len(rows))
	for i := range rows {
		sg, err := rows[i].segment(show)
		if err != nil {
			httpapi.Fail(c, err)
			return
		}
		items = append(items, sg)
	}
	if err := s.countRules(ctx, items...); err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
}

func (s *Service) get(c *gin.Context) {
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	s.render(c, id, httpapi.OK)
}

type segmentInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Discount    *string `json:"discount"`
}

// validate checks the provided fields of a segment of the kind; create
// requires name. A personal segment has no name of its own (it goes by its
// user) and no description.
func (in *segmentInput) validate(ctx context.Context, kind string, create bool) error {
	if kind == core.SegmentPersonal && (in.Name != nil || in.Description != nil) {
		return core.InvalidFields(fieldError(ctx, "name", "invalid", "a personal user group only has a discount", "个人专属只能修改折扣"))
	}
	var fe []core.FieldError
	name, given := "", in.Name != nil
	if given {
		name = strings.TrimSpace(*in.Name)
		in.Name = &name
	}
	if (given || create) && name == "" {
		fe = append(fe, fieldError(ctx, "name", "required", "name is required", "名称必填"))
	} else if utf8.RuneCountInString(name) > 100 {
		fe = append(fe, fieldError(ctx, "name", "invalid", "name must be at most 100 characters", "名称最多 100 个字符"))
	}
	if in.Description != nil && utf8.RuneCountInString(*in.Description) > 2000 {
		fe = append(fe, fieldError(ctx, "description", "too_long", "description is too long", "描述过长"))
	}
	if in.Discount != nil {
		d, ok := core.ParseRate(*in.Discount)
		if !ok || !d.IsPositive() {
			fe = append(fe, fieldError(ctx, "discount", "invalid", "must be a positive number with at most 4 decimals", "必须为正数，最多 4 位小数"))
		}
		discount := d.String()
		in.Discount = &discount
	}
	if len(fe) > 0 {
		return core.InvalidFields(fe...)
	}
	return nil
}

// columns are the segments columns of the provided fields, as written; the
// others keep their value.
func (in *segmentInput) columns() map[string]any {
	set := map[string]any{"updated_at": gorm.Expr("now()")}
	for column, v := range map[string]*string{"name": in.Name, "description": in.Description, "discount": in.Discount} {
		if v != nil {
			set[column] = *v
		}
	}
	return set
}

// nameTaken turns a clash with the name of another named segment into the
// field error; other errors pass through.
func nameTaken(ctx context.Context, err error) error {
	if store.IsUniqueViolation(err, "segments_name_uq") {
		return core.InvalidFields(fieldError(ctx, "name", "duplicate", "name already in use", "名称已被使用"))
	}
	return err
}

func (s *Service) create(c *gin.Context) {
	ctx := c.Request.Context()
	var in segmentInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if err := in.validate(ctx, core.SegmentNamed, true); err != nil {
		httpapi.Fail(c, err)
		return
	}
	discount := decimal.RequireFromString(core.ValueOr(in.Discount, "1"))
	row := model.Segment{Name: *in.Name, Description: core.ValueOr(in.Description, ""), Discount: &discount}
	if err := store.G(ctx, s.db.Pool).Create(&row).Error; err != nil {
		httpapi.Fail(c, nameTaken(ctx, err))
		return
	}
	s.render(c, row.ID, httpapi.Created)
}

func (s *Service) update(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	var in segmentInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	kind, _, err := segmentKind(ctx, s.db.Pool, id, "")
	if err == nil {
		err = in.validate(ctx, kind, false)
	}
	if err == nil && in.Discount != nil {
		err = GuardOwnSegments(ctx, s.db.Pool, s.users, []int64{id})
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if err = store.G(ctx, s.db.Pool).Model(&model.Segment{}).Where("id = ?", id).Updates(in.columns()).Error; err != nil {
		httpapi.Fail(c, nameTaken(ctx, err))
		return
	}
	s.render(c, id, httpapi.OK)
}

// delete removes a named segment with its grants, rates and model prices
// (they cascade). Its members fall back to the default segment. A personal
// segment only goes this way once its user is deleted (requireOrphan).
func (s *Service) delete(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		// The lock keeps new members out until the segment is gone.
		kind, isDefault, err := segmentKind(ctx, tx, id, "UPDATE")
		switch {
		case err != nil:
			return err
		case isDefault:
			return core.ErrConflict.WithMessage(t(ctx, "the default user group cannot be deleted", "默认用户组不能删除"))
		case kind == core.SegmentPersonal:
			if err := requireOrphan(ctx, tx, id); err != nil {
				return err
			}
		}
		// Its members fall back to the default segment's terms.
		if err := GuardOwnSegments(ctx, tx, s.users, []int64{id}); err != nil {
			return err
		}
		// Deleted users leave too: users.segment_id references the row.
		// Members are locked before they are moved, as moveUsers does.
		members, err := lockUsers(ctx, tx, `segment_id = ?`, id)
		if err != nil {
			return err
		}
		g := store.G(ctx, tx)
		if err := g.Model(&model.User{}).Where("id = ANY(?)", store.Int64s(members)).
			Update("segment_id", gorm.Expr("default_segment_id()")).Error; err != nil {
			return err
		}
		return g.Delete(&model.Segment{}, id).Error
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.priceRulesChanged(ctx)
	httpapi.NoContent(c)
}

// requireOrphan fails unless the user of the personal segment is deleted. A
// personal segment goes when its user leaves it; one a deleted user is still
// in was left by a node of the version before segments, which deletes a user
// without the user's segment, and can only be deleted itself.
func requireOrphan(ctx context.Context, q store.Querier, id int64) error {
	var live bool
	err := store.G(ctx, q).Table("segments s").Select("u.deleted_at IS NULL").Joins("JOIN users u ON u.id = s.user_id").
		Where("s.id = ?", id).Take(&live).Error
	if err == nil && live {
		return core.ErrConflict.WithMessage(t(ctx, "a personal user group is deleted by moving its user to a user group",
			"个人专属通过把用户移回用户组来删除"))
	}
	return err
}

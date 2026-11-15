package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Price is the API view of a model_prices row. Prices are global per model
// (ARCHITECTURE 7.3) and set by administrators only: typed in (manual) or
// imported from a price sync source (sync).
type Price struct {
	ID             int64           `json:"id"`
	Model          string          `json:"model"`
	Mode           string          `json:"mode"`
	Config         json.RawMessage `json:"config"`
	Expression     string          `json:"expression"`
	ExprVersion    int             `json:"expr_version"`
	ExprHash       string          `json:"expr_hash"`
	Source         string          `json:"source"`
	SyncSourceID   *int64          `json:"sync_source_id"`
	SyncSourceName *string         `json:"sync_source_name"`
	SyncedAt       *time.Time      `json:"synced_at"`
	Enabled        bool            `json:"enabled"`
	Note           string          `json:"note"`
	UpdatedBy      *int64          `json:"updated_by"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Analysis       *expr.Analysis  `json:"analysis,omitempty"`
}

const priceColumns = `mp.id, mp.model, mp.mode, mp.config, mp.expression, mp.expr_version, mp.expr_hash,
	mp.source, mp.sync_source_id, ps.name AS sync_source_name, mp.synced_at, mp.enabled, mp.note, mp.updated_by, mp.updated_at`

// pricesWithSource is the model_prices rows (alias mp) with their sync source (ps).
func pricesWithSource(db *gorm.DB) *gorm.DB {
	return db.Table("model_prices mp").Joins("LEFT JOIN price_sync_sources ps ON ps.id = mp.sync_source_id")
}

// priceRow is a row of pricesWithSource read as priceColumns.
type priceRow struct {
	model.ModelPrice
	SyncSourceName *string
}

func (r *priceRow) price() *Price {
	p := &Price{ID: r.ID, Model: r.Model, Mode: r.Mode, Config: r.Config, Expression: r.Expression,
		ExprVersion: core.ValueOr(r.ExprVersion, 0), ExprHash: r.ExprHash, Source: r.Source, SyncSourceID: r.SyncSourceID,
		SyncSourceName: r.SyncSourceName, SyncedAt: r.SyncedAt, Enabled: core.ValueOr(r.Enabled, false), Note: r.Note,
		UpdatedBy: r.UpdatedBy, UpdatedAt: r.UpdatedAt}
	if prog, err := expr.CompileCached(p.Expression); err == nil {
		p.Analysis = prog.Analyze()
	}
	return p
}

// priceByID selects the price id, locking its model_prices row when lock is
// set.
func priceByID(db *gorm.DB, id int64, lock bool) *gorm.DB {
	db = pricesWithSource(db).Select(priceColumns).Where("mp.id = ?", id)
	if lock {
		db = db.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate, Table: clause.Table{Name: "mp"}})
	}
	return db
}

func (s *Service) getPrice(ctx context.Context, q store.Querier, id int64, lock bool) (*Price, error) {
	var row priceRow
	err := priceByID(store.G(ctx, q), id, lock).Take(&row).Error
	if store.IsNoRows(err) {
		return nil, core.ErrNotFound.WithMessage("price not found")
	}
	if err != nil {
		return nil, err
	}
	return row.price(), nil
}

func (s *Service) registerPriceRoutes(r *httpapi.Router) {
	r.Perm("GET", "/prices", "price:read", s.listPrices)
	r.Perm("POST", "/prices", "price:manage", s.createPrice)
	r.Perm("POST", "/prices/validate", "price:read", s.validatePrice)
	r.Perm("POST", "/prices/preview", "price:read", s.previewPrice)
	r.Perm("GET", "/prices/history/:expr_hash", "price:read", s.priceHistory)
	r.Perm("GET", "/prices/:id", "price:read", s.showPrice)
	r.Perm("PATCH", "/prices/:id", "price:manage", s.updatePrice)
	r.Perm("DELETE", "/prices/:id", "price:manage", s.deletePrice)
}

// GET /prices?mode=&source=&sync_source_id=&enabled=&q=
func (s *Service) listPrices(c *gin.Context) {
	ctx := c.Request.Context()
	page, size := httpapi.Pagination(c)
	var f store.Conds
	if v := c.Query("mode"); v != "" {
		f.And("mp.mode = ?", v)
	}
	if v := c.Query("source"); v != "" {
		f.And("mp.source = ?", v)
	}
	if v := c.Query("sync_source_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpapi.Fail(c, core.InvalidFields(core.FieldError{Field: "sync_source_id", Code: "invalid", Message: "must be an integer"}))
			return
		}
		f.And("mp.sync_source_id = ?", id)
	}
	if v := c.Query("enabled"); v != "" {
		f.And("mp.enabled = ?", v == "true" || v == "1")
	}
	if v := strings.TrimSpace(c.Query("q")); v != "" {
		pattern := "%" + store.Like(v) + "%"
		f.And("(mp.model ILIKE ? OR mp.note ILIKE ?)", pattern, pattern)
	}
	g := store.G(ctx, s.db.Pool)
	var total int64
	if err := pricesWithSource(g).Scopes(f.Scope()).Count(&total).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	var rows []priceRow
	if err := pricePage(g, f, page, size).Scan(&rows).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	items := make([]*Price, 0, len(rows))
	for i := range rows {
		items = append(items, rows[i].price())
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
}

// pricePage is one page of the prices f selects, by model.
func pricePage(db *gorm.DB, f store.Conds, page, size int) *gorm.DB {
	return pricesWithSource(db).Select(priceColumns).Scopes(f.Scope()).Order("mp.model, mp.id").Limit(size).Offset((page - 1) * size)
}

func (s *Service) showPrice(c *gin.Context) {
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	p, err := s.getPrice(c.Request.Context(), s.db.Pool, id, false)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, p)
}

// priceInput is the body of POST/PATCH /prices.
type priceInput struct {
	Model      *string         `json:"model"`
	Mode       *string         `json:"mode"`
	Config     json.RawMessage `json:"config"`
	Expression *string         `json:"expression"`
	Enabled    *bool           `json:"enabled"`
	Note       *string         `json:"note"`
	// Confirm acknowledges the big-cost warning.
	Confirm bool `json:"confirm"`
}

// checked is a validated expression ready to store.
type checked struct {
	mode   string
	config json.RawMessage
	src    string
	prog   *expr.Program
}

// check builds and validates the expression for a price. Validation errors
// and an unconfirmed big-cost warning are returned as invalid_argument with
// details {errors, warnings, confirmation_required}.
func (s *Service) check(ctx context.Context, mode string, config json.RawMessage, expression string, confirm bool) (*checked, error) {
	if len(config) == 0 || string(config) == "null" {
		config = json.RawMessage("{}")
	}
	if !json.Valid(config) {
		return nil, core.InvalidFields(core.FieldError{Field: "config", Code: "invalid", Message: "must be a JSON object"})
	}
	src, err := expressionFor(mode, config, strings.TrimSpace(expression))
	if err != nil {
		if ce, ok := err.(*expr.ConfigError); ok {
			return nil, core.ErrInvalidArgument.WithMessage(ce.Error()).WithDetails(map[string]any{
				"fields": []core.FieldError{{Field: ce.Field, Code: ce.Issue.Code, Message: ce.Issue.Detail}},
				"errors": []expr.Issue{ce.Issue},
			})
		}
		return nil, core.InvalidFields(core.FieldError{Field: "mode", Code: "invalid", Message: err.Error()})
	}
	st, _ := s.Settings(ctx)
	rep := expr.Validate(src, expr.ValidateOptions{Facts: s.factsFor(), BigCostUSD: st.BigCostWarningUSD})
	if !rep.OK {
		return nil, core.ErrInvalidArgument.WithMessage("invalid price expression").WithDetails(map[string]any{
			"expression": src, "errors": rep.Errors, "warnings": rep.Warnings,
		})
	}
	if !confirm {
		for _, w := range rep.Warnings {
			if w.Code == expr.CodeBigCost {
				return nil, core.ErrInvalidArgument.WithMessage("confirmation required").WithDetails(map[string]any{
					"expression": src, "errors": []expr.Issue{}, "warnings": rep.Warnings, "confirmation_required": true,
				})
			}
		}
	}
	prog, err := expr.CompileCached(src)
	if err != nil {
		return nil, err
	}
	return &checked{mode: mode, config: config, src: src, prog: prog}, nil
}

// modelField is the field error for a model that is not a complete model id.
func modelField(code string) core.FieldError {
	return core.FieldError{Field: "model", Code: code,
		Message: "a complete model id: letters, digits and . _ : / @ + - (no wildcards)"}
}

func (s *Service) createPrice(c *gin.Context) {
	ctx := c.Request.Context()
	var in priceInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	var fields []core.FieldError
	model, mode := deref(in.Model), deref(in.Mode)
	switch {
	case model == "":
		fields = append(fields, modelField("required"))
	case !manifest.ValidModelID(model):
		fields = append(fields, modelField("invalid"))
	}
	if mode == "" {
		fields = append(fields, core.FieldError{Field: "mode", Code: "required", Message: "per_request, per_token or expression"})
	}
	if len(fields) > 0 {
		httpapi.Fail(c, core.InvalidFields(fields...))
		return
	}
	ck, err := s.check(ctx, mode, in.Config, deref(in.Expression), in.Confirm)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	enabled := in.Enabled == nil || *in.Enabled
	uid, _ := core.UserID(ctx)
	var p *Price
	err = s.db.Tx(ctx, func(tx pgx.Tx) error {
		row := manualPrice(model, ck, enabled, deref(in.Note), nullID(uid))
		if err := store.G(ctx, tx).Create(row).Error; err != nil {
			return err
		}
		var err error
		if p, err = s.getPrice(ctx, tx, row.ID, false); err != nil {
			return err
		}
		return recordHistory(ctx, tx, ck.prog)
	})
	if store.IsUniqueViolation(err, "") {
		httpapi.Fail(c, errPriceExists)
		return
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.changed(ctx, "prices")
	httpapi.Created(c, p)
}

func (s *Service) updatePrice(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	var in priceInput
	if !httpapi.BindJSON(c, &in) {
		return
	}
	uid, _ := core.UserID(ctx)
	var out *Price
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		cur, err := s.getPrice(ctx, tx, id, true)
		if err != nil {
			return err
		}
		model, mode := cur.Model, cur.Mode
		if in.Model != nil {
			model = *in.Model
		}
		if in.Mode != nil {
			mode = *in.Mode
		}
		if !manifest.ValidModelID(model) {
			return core.InvalidFields(modelField("invalid"))
		}
		config, expression := cur.Config, cur.Expression
		pricing := in.Mode != nil || in.Config != nil || in.Expression != nil
		if in.Config != nil {
			config = in.Config
		}
		if in.Expression != nil {
			expression = *in.Expression
		} else if in.Config != nil && mode == expr.ModeExpression && expr.HasVisualConfig(in.Config) {
			expression = "" // regenerate from the new visual config
		}
		src, hash, version := cur.Expression, cur.ExprHash, cur.ExprVersion
		if pricing {
			ck, err := s.check(ctx, mode, config, expression, in.Confirm)
			if err != nil {
				return err
			}
			config, src, hash, version = ck.config, ck.src, ck.prog.Hash(), ck.prog.Version()
			if err := recordHistory(ctx, tx, ck.prog); err != nil {
				return err
			}
		}
		enabled, note := cur.Enabled, cur.Note
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		if in.Note != nil {
			note = *in.Note
		}
		// Editing a synced price makes it manual: later syncs only replace
		// it when an administrator selects it in the preview.
		source, syncID := cur.Source, cur.SyncSourceID
		if pricing || in.Model != nil {
			source, syncID = SourceManual, nil
		}
		if err := updatePriceRow(store.G(ctx, tx), id, map[string]any{
			"model": model, "mode": mode, "config": config, "expression": src, "expr_version": version,
			"expr_hash": hash, "enabled": enabled, "note": note, "updated_by": nullID(uid),
			"source": source, "sync_source_id": syncID,
		}).Error; err != nil {
			return err
		}
		out, err = s.getPrice(ctx, tx, id, false)
		return err
	})
	if store.IsUniqueViolation(err, "") {
		httpapi.Fail(c, errPriceExists)
		return
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.changed(ctx, "prices")
	httpapi.OK(c, out)
}

func (s *Service) deletePrice(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	if _, err := s.getPrice(ctx, s.db.Pool, id, false); err != nil {
		httpapi.Fail(c, err)
		return
	}
	if err := store.G(ctx, s.db.Pool).Where("id = ?", id).Delete(&model.ModelPrice{}).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.changed(ctx, "prices")
	httpapi.NoContent(c)
}

// manualPrice is a price an administrator typed in, as created. Every
// column is written, enabled and expr_version included; updated_at is the
// default.
func manualPrice(name string, ck *checked, enabled bool, note string, by *int64) *model.ModelPrice {
	version := ck.prog.Version()
	return &model.ModelPrice{Model: name, Mode: ck.mode, Config: ck.config, Expression: ck.src, ExprVersion: &version,
		ExprHash: ck.prog.Hash(), Source: SourceManual, Enabled: &enabled, Note: note, UpdatedBy: by}
}

// updatePriceRow writes the columns of set to the price id and moves its
// updated_at, as every edit of a price does.
func updatePriceRow(db *gorm.DB, id int64, set map[string]any) *gorm.DB {
	set["updated_at"] = gorm.Expr("now()")
	return db.Model(&model.ModelPrice{}).Where("id = ?", id).Updates(set)
}

var errPriceExists = core.ErrConflict.WithMessage("a price for this model already exists")

func (s *Service) priceHistory(c *gin.Context) {
	ctx := c.Request.Context()
	hash := c.Param("expr_hash")
	var row model.ModelPriceHistory
	err := store.G(ctx, s.db.Pool).Where("expr_hash = ?", hash).Take(&row).Error
	if store.IsNoRows(err) {
		httpapi.Fail(c, core.ErrNotFound.WithMessage("expression not found"))
		return
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, struct {
		ExprHash    string    `json:"expr_hash"`
		Expression  string    `json:"expression"`
		ExprVersion int       `json:"expr_version"`
		CreatedAt   time.Time `json:"created_at"`
	}{row.ExprHash, row.Expression, row.ExprVersion, row.CreatedAt})
}

// POST /prices/validate {mode, config?, expression?}
func (s *Service) validatePrice(c *gin.Context) {
	ctx := c.Request.Context()
	var in struct {
		Mode       string          `json:"mode"`
		Config     json.RawMessage `json:"config"`
		Expression string          `json:"expression"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	if in.Mode == "" {
		in.Mode = expr.ModeExpression
	}
	if len(in.Config) == 0 || string(in.Config) == "null" {
		in.Config = json.RawMessage("{}")
	}
	src, err := expressionFor(in.Mode, in.Config, strings.TrimSpace(in.Expression))
	if err != nil {
		issue := expr.Issue{Code: expr.CodeConfig, Message: map[string]string{"en": err.Error(), "zh": err.Error()}}
		if ce, ok := err.(*expr.ConfigError); ok {
			issue = ce.Issue
		}
		httpapi.OK(c, &expr.Report{Errors: []expr.Issue{issue}, Warnings: []expr.Issue{}})
		return
	}
	st, _ := s.Settings(ctx)
	httpapi.OK(c, expr.Validate(src, expr.ValidateOptions{Facts: s.factsFor(), BigCostUSD: st.BigCostWarningUSD}))
}

// PreviewRequest is the body of POST /prices/preview.
type PreviewRequest struct {
	PriceID    *int64            `json:"price_id"`
	Mode       string            `json:"mode"`
	Config     json.RawMessage   `json:"config"`
	Expression string            `json:"expression"`
	Usage      PreviewUsage      `json:"usage"`
	Metrics    map[string]any    `json:"metrics"`
	Headers    map[string]string `json:"headers"`
	Params     map[string]any    `json:"params"`
	At         *time.Time        `json:"at"`
	GroupID    *int64            `json:"group_id"`
}

// PreviewUsage holds token counts in the exclusive form (p excludes cache).
// They are normalized like settlement (expr.Normalize): cache categories the
// expression does not price are not billed. len defaults to
// p + cr + cc + cc1h.
type PreviewUsage struct {
	P    float64  `json:"p"`
	C    float64  `json:"c"`
	CR   float64  `json:"cr"`
	CC   float64  `json:"cc"`
	CC1h float64  `json:"cc1h"`
	Len  *float64 `json:"len"`
}

// PreviewResult is the response of POST /prices/preview.
type PreviewResult struct {
	Cost           decimal.Decimal   `json:"cost"`      // after the group multiplier
	BaseCost       decimal.Decimal   `json:"base_cost"` // before the group multiplier
	RateMultiplier decimal.Decimal   `json:"rate_multiplier"`
	Tier           string            `json:"tier"`
	Rules          []expr.RuleResult `json:"rules"`
	Breakdown      expr.Breakdown    `json:"breakdown"`
	Expression     string            `json:"expression"`
	ExprHash       string            `json:"expr_hash"`
}

func (s *Service) previewPrice(c *gin.Context) {
	ctx := c.Request.Context()
	var in PreviewRequest
	if !httpapi.BindJSON(c, &in) {
		return
	}
	src := in.Expression
	if in.PriceID != nil {
		p, err := s.getPrice(ctx, s.db.Pool, *in.PriceID, false)
		if err != nil {
			httpapi.Fail(c, err)
			return
		}
		src = p.Expression
	} else {
		mode := in.Mode
		if mode == "" {
			mode = expr.ModeExpression
		}
		var err error
		if src, err = expressionFor(mode, in.Config, strings.TrimSpace(in.Expression)); err != nil {
			httpapi.Fail(c, core.ErrInvalidArgument.WithMessage(err.Error()))
			return
		}
	}
	prog, err := expr.CompileCached(src)
	if err != nil {
		details := map[string]any{"expression": src}
		if ce, ok := err.(*expr.CompileError); ok {
			details["errors"] = []expr.Issue{ce.Issue}
		}
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("invalid price expression").WithDetails(details))
		return
	}
	u := in.Usage
	vars := expr.Normalize(expr.SemanticsExclusive, expr.Tokens{
		Input: int64(u.P), Output: int64(u.C), CacheRead: int64(u.CR), CacheCreation: int64(u.CC), CacheCreation1h: int64(u.CC1h),
	}, prog.Uses)
	if u.Len != nil {
		vars.Len = *u.Len
	}
	input := expr.Input{Vars: vars, Metrics: in.Metrics, Headers: map[string]string{}, Params: map[string]string{}}
	for k, v := range in.Headers {
		input.Headers[strings.ToLower(k)] = v
	}
	for k, v := range in.Params {
		b, _ := json.Marshal(v)
		input.Params[k] = string(b)
	}
	if in.At != nil {
		input.At = *in.At
	}
	res, err := prog.Eval(input)
	if err != nil {
		details := map[string]any{"expression": src}
		if ee, ok := err.(*expr.EvalError); ok {
			details["errors"] = []expr.Issue{ee.Issue}
		}
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage(err.Error()).WithDetails(details))
		return
	}
	mult := decimal.NewFromInt(1)
	if in.GroupID != nil {
		var g model.Group
		err := store.G(ctx, s.db.Pool).Select("rate_multiplier").Where("id = ?", *in.GroupID).Take(&g).Error
		if store.IsNoRows(err) {
			httpapi.Fail(c, core.ErrNotFound.WithMessage("group not found"))
			return
		}
		if err != nil {
			httpapi.Fail(c, err)
			return
		}
		mult = core.ValueOr(g.RateMultiplier, mult) // NOT NULL: always read
	}
	httpapi.OK(c, &PreviewResult{
		Cost:           FinalCost(res.Cost, mult),
		BaseCost:       res.Cost,
		RateMultiplier: mult,
		Tier:           res.Tier,
		Rules:          res.Rules,
		Breakdown:      res.Breakdown,
		Expression:     prog.Expression(),
		ExprHash:       prog.Hash(),
	})
}

// FinalCost applies the group multiplier and rounds to the 8 decimal places
// stored in the database.
func FinalCost(cost, rateMultiplier decimal.Decimal) decimal.Decimal {
	return cost.Mul(rateMultiplier).Round(8)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// expressionFor returns the expression stored for a price: generated from
// the visual config, or the hand-written expression in expression mode.
func expressionFor(mode string, config json.RawMessage, expression string) (string, error) {
	switch mode {
	case expr.ModePerRequest, expr.ModePerToken:
		return expr.Generate(mode, config)
	case expr.ModeExpression:
		var visual expr.ExpressionConfig
		if json.Unmarshal(config, &visual) == nil && visual.Video != nil {
			return expr.Generate(mode, config)
		}
		if expression != "" {
			return expression, nil
		}
		return expr.Generate(mode, config)
	}
	return "", fmt.Errorf("unknown mode %q", mode)
}

// recordHistorySQL keeps an expression (hash, canonical text, version) where
// a settlement retry reads it back, with the arguments historyArgs gives.
// recordHistory runs it through GORM; queueSave batches the same text
// numbered for pgx (recordHistoryBatchSQL), so the two cannot drift apart.
const recordHistorySQL = `
		INSERT INTO model_price_history (expr_hash, expression, expr_version) VALUES (?, ?, ?)
		ON CONFLICT (expr_hash) DO NOTHING`

var recordHistoryBatchSQL = pgxNumbered(recordHistorySQL)

func historyArgs(p *expr.Program) []any { return []any{p.Hash(), p.Expression(), p.Version()} }

// recordHistory runs recordHistorySQL on q.
func recordHistory(ctx context.Context, q store.Querier, p *expr.Program) error {
	return insertHistory(store.G(ctx, q), p).Error
}

func insertHistory(db *gorm.DB, p *expr.Program) *gorm.DB {
	return db.Exec(recordHistorySQL, historyArgs(p)...)
}

// pgxNumbered numbers the ? placeholders of a statement $1, $2 ... for a
// pgx.Batch. The text must hold no other ? (a jsonb operator, a literal).
func pgxNumbered(query string) string {
	parts := strings.Split(query, "?")
	var b strings.Builder
	b.WriteString(parts[0])
	for i, part := range parts[1:] {
		b.WriteString("$" + strconv.Itoa(i+1))
		b.WriteString(part)
	}
	return b.String()
}

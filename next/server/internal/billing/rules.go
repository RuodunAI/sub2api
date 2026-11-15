package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/billing/expr"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Rule is a model price of a group, or of a segment in a group, as the API
// reads and writes it (CONTRACTS §97).
type Rule struct {
	Kind string `json:"kind"`
	// Multiplier (kind multiplier), a decimal: the list price is the global
	// price times it, and follows the global price when that changes.
	Multiplier *string `json:"multiplier,omitempty"`
	// Mode and Config (kind custom): a price of its own in the mode of the
	// global price, per_token (p, c, cr, cc, cc1h) or per_request (price).
	Mode   string              `json:"mode,omitempty"`
	Config map[string]*float64 `json:"config,omitempty"`
	// Inactive marks a custom price in another mode than the global price has
	// now: it is skipped and the next level prices the model. Output only.
	Inactive bool `json:"inactive,omitempty"`
}

// storedRule is a price_rules row: the rule and, of a custom price, the
// expression generated from its config when it was saved.
type storedRule struct {
	Rule
	scale       decimal.Decimal // Multiplier, as a number
	expression  string
	exprHash    string
	exprVersion int
}

// setScale makes n the multiplier of the rule.
func (r *storedRule) setScale(n decimal.Decimal) {
	text := n.String()
	r.scale, r.Multiplier = n, &text
}

// inactive reports whether the rule does not apply over a global price of
// mode: a custom price keeps the mode it was saved in.
func (r *storedRule) inactive(mode string) bool {
	return r.Kind == core.PriceRuleCustom && r.Mode != mode
}

// view returns the rule as the API shows it over the global price base
// (nil = the model has none).
func (r *storedRule) view(base *PriceDef) *Rule {
	if r == nil {
		return nil
	}
	out := r.Rule
	out.Inactive = base != nil && r.inactive(base.Mode)
	return &out
}

// price returns the list price the rule makes of the global one, with where
// the rule is from: a multiplier leaves the expression and sets Scale; a
// custom price puts its own expression in and keeps the global one as Base.
// ID and VideoOnly are always the global price's. A nil rule is the global
// price as it is, its Source left empty (core.PriceRule.Source).
func (r *storedRule) price(global core.PriceRule, source string) *core.PriceRule {
	if r == nil {
		return &global
	}
	global.Source = source
	if r.Kind == core.PriceRuleMultiplier {
		scale := r.scale
		global.Scale = &scale
		return &global
	}
	global.Base = &core.PriceBase{Expression: global.Expression, ExprHash: global.ExprHash}
	global.Mode, global.Expression, global.ExprHash, global.ExprVersion = r.Mode, r.expression, r.exprHash, r.exprVersion
	return &global
}

// ruleLevels are the rules on one model in one group: that of a segment and
// the group's own.
type ruleLevels struct{ segment, group *storedRule }

// applied returns the first rule that applies over a global price of mode,
// the segment's before the group's, and where the list price then comes from.
// It is the one place the levels are ordered.
func (l ruleLevels) applied(mode string) (*storedRule, string) {
	switch {
	case l.segment != nil && !l.segment.inactive(mode):
		return l.segment, core.PriceSourceSegment
	case l.group != nil && !l.group.inactive(mode):
		return l.group, core.PriceSourceGroup
	}
	return nil, core.PriceSourceGlobal
}

// ruleSet is price rules by what they are on; SegmentID 0 is the group's own
// rule on the model.
type ruleSet map[core.PriceQuery]*storedRule

// levels returns the rules that may price q.
func (rs ruleSet) levels(q core.PriceQuery) ruleLevels {
	l := ruleLevels{group: rs[core.PriceQuery{Model: q.Model, GroupID: q.GroupID}]}
	if q.SegmentID != 0 {
		l.segment = rs[q]
	}
	return l
}

// rulesFor selects the rules that may price the models of a group for a
// segment: the segment's own and the group's. It takes the group's id, then
// the segment's twice.
const rulesFor = `group_id = ? AND COALESCE(segment_id, ?) = ?`

// ruleRow is a row of priceRules.
type ruleRow struct {
	GroupID    int64
	SegmentID  int64
	Model      string
	Kind       string
	Multiplier *decimal.Decimal
	Mode       string
	Config     store.JSON[map[string]*float64]
	Expression string
	ExprHash   string
}

// priceRules selects the price rules cond selects; "" selects all of them.
func priceRules(db *gorm.DB, cond string, args ...any) *gorm.DB {
	db = db.Model(&model.PriceRule{}).Select(`group_id, COALESCE(segment_id, 0) AS segment_id, model, kind, multiplier,
		COALESCE(mode, '') AS mode, config, expression, expr_hash`)
	if cond == "" {
		return db
	}
	return db.Where(cond, args...)
}

// loadRules reads the price rules cond selects ("" for all of them).
func loadRules(ctx context.Context, q *gorm.DB, cond string, args ...any) (ruleSet, error) {
	var rows []ruleRow
	if err := priceRules(store.G(ctx, q), cond, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(ruleSet, len(rows))
	for _, row := range rows {
		on := core.PriceQuery{GroupID: row.GroupID, SegmentID: row.SegmentID, Model: row.Model}
		r := &storedRule{Rule: Rule{Kind: row.Kind, Mode: row.Mode, Config: row.Config.V},
			expression: row.Expression, exprHash: row.ExprHash}
		if row.Multiplier != nil {
			r.setScale(*row.Multiplier)
		}
		out[on] = r
		// Only a custom price has an expression of its own.
		if r.Kind != core.PriceRuleCustom {
			continue
		}
		if prog, err := expr.CompileCached(r.expression); err == nil {
			r.exprVersion = prog.Version()
		}
	}
	return out, nil
}

// customPrices are the prices a custom rule may set per mode; the first
// required of them must be set. The console checks a rule by the same table
// before it saves.
var customPrices = map[string]struct {
	keys     []string
	required int
}{
	expr.ModePerRequest: {[]string{"price"}, 1},
	expr.ModePerToken:   {[]string{"p", "c", "cr", "cc", "cc1h"}, 2},
}

// checkRule validates a rule over the global price of its model (nil = the
// model has none) and returns it as it is stored. The error says why it
// cannot be saved.
func checkRule(ctx context.Context, in Rule, base *PriceDef) (*storedRule, error) {
	if base == nil {
		return nil, errors.New(core.Text(ctx, "the model has no global price", "该模型没有全局价格"))
	}
	switch in.Kind {
	case core.PriceRuleMultiplier:
		scale, ok := core.ParseRate(deref(in.Multiplier))
		if !ok {
			return nil, errors.New(core.Text(ctx, "multiplier must be 0 or more and below 1000000, with at most 4 decimals",
				"倍数必须不小于 0、小于 1000000，最多 4 位小数"))
		}
		r := &storedRule{Rule: Rule{Kind: in.Kind}}
		r.setScale(scale)
		return r, nil
	case core.PriceRuleCustom:
		return checkCustom(ctx, in, base.Mode)
	}
	return nil, errors.New(core.Text(ctx, "rule must be a multiplier or a custom price", "规则必须是倍数或自定义价"))
}

// checkCustom validates a custom price over a global price of mode and
// generates its expression. Only the prices of the mode are kept.
func checkCustom(ctx context.Context, in Rule, mode string) (*storedRule, error) {
	prices, ok := customPrices[mode]
	if !ok {
		return nil, errors.New(core.Text(ctx, "an expression price takes a multiplier only", "表达式计费的模型只能用倍数规则"))
	}
	if in.Mode != mode {
		return nil, fmt.Errorf(core.Text(ctx, "a custom price is %s, as the global price", "自定义价的计费方式须与全局价相同：%s"), mode)
	}
	config := make(map[string]*float64, len(prices.keys))
	for _, key := range prices.keys {
		if v := in.Config[key]; v != nil && *v < 0 {
			return nil, errors.New(core.Text(ctx, "prices must be non-negative numbers", "价格必须为非负数"))
		}
		config[key] = in.Config[key]
	}
	for _, key := range prices.keys[:prices.required] {
		if config[key] == nil {
			return nil, fmt.Errorf(core.Text(ctx, "%s required", "%s 必填"), strings.Join(prices.keys[:prices.required], ", "))
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	src, err := expr.Generate(mode, raw)
	if err != nil {
		return nil, err
	}
	prog, err := expr.CompileCached(src)
	if err != nil {
		return nil, err
	}
	return &storedRule{Rule: Rule{Kind: in.Kind, Mode: mode, Config: config},
		expression: src, exprHash: prog.Hash(), exprVersion: prog.Version()}, nil
}

// saveRule stores the rule on on, replacing the one there. The expression of
// a custom price also goes to model_price_history, as a global price's does:
// a settlement retry reads it back by expr_hash on any node.
func saveRule(db *gorm.DB, on core.PriceQuery, r *storedRule, updatedBy int64) error {
	if r.Kind == core.PriceRuleCustom {
		prog, err := expr.CompileCached(r.expression)
		if err != nil {
			return err
		}
		if err := insertHistory(db, prog).Error; err != nil {
			return err
		}
	}
	return upsertRule(db, on, r, updatedBy).Error
}

// upsertRule writes the rule on on over the one there, if any.
func upsertRule(db *gorm.DB, on core.PriceQuery, r *storedRule, updatedBy int64) *gorm.DB {
	target := `(group_id, model) WHERE segment_id IS NULL`
	if on.SegmentID != 0 {
		target = `(group_id, segment_id, model) WHERE segment_id IS NOT NULL`
	}
	return db.Exec(`
		INSERT INTO price_rules (group_id, segment_id, model, kind, multiplier, mode, config, expression, expr_hash, updated_by)
		VALUES (?, ?, ?, ?, ?::numeric, NULLIF(?, ''), ?, ?, ?, ?)
		ON CONFLICT `+target+` DO UPDATE SET kind = EXCLUDED.kind, multiplier = EXCLUDED.multiplier,
			mode = EXCLUDED.mode, config = EXCLUDED.config, expression = EXCLUDED.expression,
			expr_hash = EXCLUDED.expr_hash, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		on.GroupID, nullID(on.SegmentID), on.Model, r.Kind, r.Multiplier, r.Mode, r.Config, r.expression, r.exprHash, nullID(updatedBy))
}

// deleteRule removes the rule on on, if there is one.
func deleteRule(db *gorm.DB, on core.PriceQuery) *gorm.DB {
	return db.Exec(`DELETE FROM price_rules WHERE group_id = ? AND model = ?
		AND segment_id IS NOT DISTINCT FROM ?`, on.GroupID, on.Model, nullID(on.SegmentID))
}

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// loadRules reads the price rules cond selects ("" for all of them).
func loadRules(ctx context.Context, q *gorm.DB, cond string, args ...any) (ruleSet, error) {
	db := store.G(ctx, q).Model(&model.PriceRule{}).Select(`group_id, COALESCE(segment_id, 0) AS segment_id, model, kind, multiplier,
		COALESCE(mode, '') AS mode, config, expression, expr_hash`)
	if cond != "" {
		db = db.Where(cond, args...)
	}
	var rows []ruleRow
	if err := db.Scan(&rows).Error; err != nil {
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

// writeRulesSQL writes rule changes in one statement and one round trip,
// as the statements of each change did one after the other: a rule removed
// is deleted where it is, a rule saved is upserted on the partial unique
// index of its level (the group's own, segment_id NULL, or a segment's), and
// the expression of a custom price also goes to model_price_history, as a
// global price's does: a settlement retry reads it back by expr_hash on any
// node.
//
// Its parameters are arrays (ruleWrites), one row per rule, whatever the
// number of changes. A row cannot be written twice in one statement, so the
// changes of one rule come collapsed (collapseChanges): removed (a delete
// came before the last change or is the last one) and saved (the last
// change saves). Deleting and then inserting the rule leaves a new row, as
// the separate statements did; the insert does not conflict with the row
// the statement deleted.
//
// The writes happen in a fixed order, each step reading the previous one
// through a scalar subquery of its WHERE clause, which PostgreSQL runs once
// before the step's first row: the history, then the existing rows of every
// changed rule locked FOR UPDATE in the order of the changes (that of
// applyChanges, so that two requests changing the same rules lock them in
// the same order and neither waits for the other in a circle), then the
// deletes, then the inserts of the group's own rules, then the segments',
// each in the order of the changes.
const writeRulesSQL = `
WITH history AS (
    INSERT INTO model_price_history (expr_hash, expression, expr_version)
    SELECT h, e, v FROM unnest(?::varchar[], ?::text[], ?::int[]) WITH ORDINALITY AS x(h, e, v, o)
    ORDER BY o
    ON CONFLICT (expr_hash) DO NOTHING
    RETURNING 1
), changed AS (
    SELECT * FROM unnest(?::bigint[], ?::bigint[], ?::text[], ?::boolean[], ?::boolean[], ?::text[], ?::text[], ?::text[],
        ?::jsonb[], ?::text[], ?::text[])
        WITH ORDINALITY AS c(group_id, segment_id, model, removed, saved, kind, multiplier, mode, config, expression, expr_hash, o)
), locked AS (
    SELECT r.id, c.removed FROM price_rules r
    JOIN changed c ON r.group_id = c.group_id AND r.model = c.model AND r.segment_id IS NOT DISTINCT FROM c.segment_id
    WHERE (SELECT count(*) FROM history) >= 0
    ORDER BY c.o
    FOR UPDATE OF r
), deleted AS (
    DELETE FROM price_rules
    WHERE id IN (SELECT id FROM locked WHERE removed) AND (SELECT count(*) FROM locked) >= 0
    RETURNING 1
), group_saved AS (
    ` + ruleInsert + `
    WHERE saved AND segment_id IS NULL AND (SELECT count(*) FROM deleted) >= 0
    ORDER BY o
    ON CONFLICT (group_id, model) WHERE segment_id IS NULL ` + ruleUpdate + `
), segment_saved AS (
    ` + ruleInsert + `
    WHERE saved AND segment_id IS NOT NULL AND (SELECT count(*) FROM group_saved) >= 0
    ORDER BY o
    ON CONFLICT (group_id, segment_id, model) WHERE segment_id IS NOT NULL ` + ruleUpdate + `
)
SELECT count(*) FROM segment_saved`

// ruleInsert and ruleUpdate are a level's upsert of the saved rules of
// writeRulesSQL: the rule over the one there, if any, by the user of the
// last parameter.
const (
	ruleInsert = `INSERT INTO price_rules (group_id, segment_id, model, kind, multiplier, mode, config, expression, expr_hash, updated_by)
    SELECT group_id, segment_id, model, kind, multiplier::numeric, NULLIF(mode, ''), config, expression, expr_hash, ?::bigint
    FROM changed`
	ruleUpdate = `DO UPDATE SET kind = EXCLUDED.kind, multiplier = EXCLUDED.multiplier,
        mode = EXCLUDED.mode, config = EXCLUDED.config, expression = EXCLUDED.expression,
        expr_hash = EXCLUDED.expr_hash, updated_by = EXCLUDED.updated_by, updated_at = now()
    RETURNING 1`
)

// ruleWrites are writeRulesSQL's arguments: the expressions for the
// history, then one element per changed rule in each array of the rules.
type ruleWrites struct {
	history []*expr.Program

	groups                      store.Int64s
	segments                    store.Array[*int64]
	models                      store.Strings
	removed, saved              store.Bools
	kinds                       store.Strings
	multipliers                 store.Array[*string]
	modes                       store.Strings
	configs                     store.Array[store.Bytes]
	ruleExpressions, ruleHashes store.Strings
}

// addHistory adds the expression of a custom price r to the history.
func (w *ruleWrites) addHistory(r *storedRule) error {
	if r.Kind != core.PriceRuleCustom {
		return nil
	}
	prog, err := expr.CompileCached(r.expression)
	if err != nil {
		return err
	}
	w.history = append(w.history, prog)
	return nil
}

// addRule adds the rule on on: removed first when removed, then saved as r
// when r is not nil.
func (w *ruleWrites) addRule(on core.PriceQuery, removed bool, r *storedRule) error {
	saved := r != nil
	if !saved {
		r = &storedRule{}
	}
	var config store.Bytes
	if r.Config != nil {
		doc, err := json.Marshal(r.Config)
		if err != nil {
			return err
		}
		config = doc
	}
	w.groups = append(w.groups, on.GroupID)
	w.segments = append(w.segments, nullID(on.SegmentID))
	w.models = append(w.models, on.Model)
	w.removed = append(w.removed, removed)
	w.saved = append(w.saved, saved)
	w.kinds = append(w.kinds, r.Kind)
	w.multipliers = append(w.multipliers, r.Multiplier)
	w.modes = append(w.modes, r.Mode)
	w.configs = append(w.configs, config)
	w.ruleExpressions = append(w.ruleExpressions, r.expression)
	w.ruleHashes = append(w.ruleHashes, r.exprHash)
	return nil
}

// writeRules runs writeRulesSQL for w on db, by updatedBy. The history goes
// in the order of its hashes: two requests that add the same new
// expressions then insert them in the same order.
func writeRules(db *gorm.DB, w *ruleWrites, updatedBy int64) *gorm.DB {
	slices.SortStableFunc(w.history, func(a, b *expr.Program) int { return strings.Compare(a.Hash(), b.Hash()) })
	hashes, expressions, versions := make(store.Strings, len(w.history)), make(store.Strings, len(w.history)), make(store.Array[int], len(w.history))
	for i, p := range w.history {
		hashes[i], expressions[i], versions[i] = p.Hash(), p.Expression(), p.Version()
	}
	return db.Exec(writeRulesSQL, hashes, expressions, versions,
		w.groups, w.segments, w.models, w.removed, w.saved, w.kinds, w.multipliers, w.modes,
		w.configs, w.ruleExpressions, w.ruleHashes, nullID(updatedBy), nullID(updatedBy))
}

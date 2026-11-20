package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Cost sources (costs.source, CONTRACTS §72.13.3).
const (
	SourceAudit = "audit"
	// SourceAnalysis is the retired AI analysis (removed in 0.10.0): cost
	// rows written before then keep it until the stats retention drops them.
	SourceAnalysis = "analysis"
	SourceTest     = "test"
	SourceTry      = "try"
	SourceSimulate = "simulate"
)

// Price modes of one call.
const (
	PricePriced   = "priced"
	PriceUnpriced = "unpriced"
	PriceFree     = "free"
)

// CallCost is the price of one metered call (or of several summed).
type CallCost struct {
	Mode         string `json:"mode"`
	Cost         string `json:"cost,omitempty"` // USD, 8 places; empty when not priced
	Reason       string `json:"reason,omitempty"`
	BillingModel string `json:"billing_model,omitempty"`
	TokensIn     int64  `json:"tokens_in"`
	TokensOut    int64  `json:"tokens_out"`
	units        int64  // cost in 1e-8 USD
}

// costUnit is 1e-8 USD, the precision the core prices with.
const costUnit int64 = 100_000_000

// parseCost reads a decimal string ("0.00001230") into 1e-8 USD units.
func parseCost(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 8 {
		frac = frac[:8]
	}
	frac += strings.Repeat("0", 8-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w > math.MaxInt64/costUnit-1 {
		return 0, false
	}
	v := w*costUnit + f
	if neg {
		v = -v
	}
	return v, true
}

// formatCost renders 1e-8 USD units as a decimal string with 8 places.
func formatCost(units int64) string {
	sign := ""
	if units < 0 {
		sign, units = "-", -units
	}
	return fmt.Sprintf("%s%d.%08d", sign, units/costUnit, units%costUnit)
}

// metered reports whether a call has a known cost: it succeeded, or failed
// after the upstream reported usage.
func metered(res EngineResult, err error) bool {
	if errors.Is(err, ErrNoInput) || errors.Is(err, errNoChannel) {
		return false
	}
	u := res.Usage
	return err == nil || u.InputTokens+u.OutputTokens+u.CacheRead+u.CacheCreation > 0
}

// billingModel is the model a channel's calls are priced on.
func (ch *liveChannel) billingModel() string {
	if ch.BillingModel != "" {
		return ch.BillingModel
	}
	return ch.Model
}

// tokens returns the input (cache included) and output tokens of a usage.
func tokensOf(u EngineUsage) (in, out int64) {
	in = u.InputTokens
	if u.Semantics != SemanticsInclusive {
		in += u.CacheRead + u.CacheCreation
	}
	return in, u.OutputTokens
}

// quote prices one call of ch with the core prices (PriceUsage). OpenAI
// moderation is free; TypeSafe bills the input only.
func (p *Plugin) quote(ctx context.Context, ch *liveChannel, u EngineUsage) CallCost {
	c := CallCost{BillingModel: ch.billingModel()}
	c.TokensIn, c.TokensOut = tokensOf(u)
	switch ch.Type {
	case ChannelOpenAI:
		c.Mode = PriceFree
		return c
	case ChannelTypeSafe:
		u = EngineUsage{InputTokens: u.InputTokens, Semantics: SemanticsInclusive}
	}
	if p.host == nil {
		c.Mode, c.Reason = PriceUnpriced, "unavailable"
		return c
	}
	sem := u.Semantics
	if sem == "" {
		sem = SemanticsInclusive
	}
	q, err := p.host.PriceUsage(ctx, pluginsdk.PriceUsage{Model: c.BillingModel, Semantics: sem,
		Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation})
	switch {
	case err != nil:
		c.Mode, c.Reason = PriceUnpriced, "unavailable"
	case !q.Priced:
		c.Mode, c.Reason = PriceUnpriced, q.Reason
	default:
		units, ok := parseCost(q.Cost)
		if !ok || units < 0 {
			c.Mode, c.Reason = PriceUnpriced, "eval_error"
			break
		}
		c.Mode, c.Cost, c.units = PricePriced, formatCost(units), units
	}
	return c
}

// ---------------------------------------------------------------- price state

// PriceState is the current pricing of a channel's billing model.
type PriceState struct {
	Priced       bool   `json:"priced"`
	Free         bool   `json:"free,omitempty"`
	Reason       string `json:"reason,omitempty"`
	BillingModel string `json:"billing_model"`
}

type priceStateEntry struct {
	state PriceState
	at    time.Time
}

// priceState asks the core whether the model has a price (cached 60 s).
func (p *Plugin) priceState(ctx context.Context, typ, model string) PriceState {
	if typ == ChannelOpenAI {
		return PriceState{Free: true, BillingModel: model}
	}
	p.priceMu.Lock()
	e, ok := p.priceCache[model]
	p.priceMu.Unlock()
	if ok && p.now().Sub(e.at) < time.Minute {
		return e.state
	}
	c := p.quote(ctx, &liveChannel{Type: typ, Model: model}, EngineUsage{Semantics: SemanticsInclusive})
	st := PriceState{Priced: c.Mode == PricePriced, Reason: c.Reason, BillingModel: model}
	if c.Reason != "unavailable" {
		p.priceMu.Lock()
		p.priceCache[model] = priceStateEntry{state: st, at: p.now()}
		p.priceMu.Unlock()
	}
	return st
}

// ---------------------------------------------------------------- GET /costs

// CostSummary adds up metered calls. The cost is kept in 1e-8 USD units and
// rendered as "cost": USD with 8 places, so the zero value is valid. Calls
// counts metered calls (priced + unpriced + free).
type CostSummary struct {
	units          int64
	Calls          int64 `json:"calls"`
	PricedCalls    int64 `json:"priced_calls"`
	UnpricedCalls  int64 `json:"unpriced_calls"`
	UnpricedTokens int64 `json:"unpriced_tokens"`
	FreeCalls      int64 `json:"free_calls"`
	TokensIn       int64 `json:"tokens_in"`
	TokensOut      int64 `json:"tokens_out"`
}

type costSummaryJSON struct {
	Cost string `json:"cost"`
	costSummaryFields
}

type costSummaryFields CostSummary

// Cost is the USD amount with 8 places.
func (s CostSummary) Cost() string { return formatCost(s.units) }

func (s CostSummary) MarshalJSON() ([]byte, error) {
	return json.Marshal(costSummaryJSON{Cost: s.Cost(), costSummaryFields: costSummaryFields(s)})
}

func (s *CostSummary) UnmarshalJSON(b []byte) error {
	var v costSummaryJSON
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = CostSummary(v.costSummaryFields)
	units, ok := parseCost(v.Cost)
	if !ok && strings.TrimSpace(v.Cost) != "" {
		return fmt.Errorf("invalid cost %q", v.Cost)
	}
	s.units = units
	return nil
}

func (s *CostSummary) add(o CostSummary) {
	s.units += o.units
	s.Calls += o.Calls
	s.PricedCalls += o.PricedCalls
	s.UnpricedCalls += o.UnpricedCalls
	s.UnpricedTokens += o.UnpricedTokens
	s.FreeCalls += o.FreeCalls
	s.TokensIn += o.TokensIn
	s.TokensOut += o.TokensOut
}

// ChannelCost is one row of by_channel.
type ChannelCost struct {
	ChannelID    int64       `json:"channel_id"`
	Name         string      `json:"name"`
	Type         string      `json:"type"`
	Model        string      `json:"model"`
	BillingModel string      `json:"billing_model"`
	Deleted      bool        `json:"deleted"`
	Summary      CostSummary `json:"summary"`
}

// SourceCost is one row of by_source.
type SourceCost struct {
	Source  string      `json:"source"`
	Summary CostSummary `json:"summary"`
}

// CostPoint is one bucket of the trend.
type CostPoint struct {
	Bucket time.Time `json:"bucket"`
	Cost   string    `json:"cost"`
}

// CostReport is GET /costs.
type CostReport struct {
	Range     string        `json:"range"`
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	Bucket    string        `json:"bucket"`
	Totals    CostSummary   `json:"totals"`
	ByChannel []ChannelCost `json:"by_channel"`
	BySource  []SourceCost  `json:"by_source"`
	Trend     []CostPoint   `json:"trend"`
	// DataFrom is set when stats_retention_days keeps less than the range:
	// the statistics before it are deleted, so the sums start there
	// (CONTRACTS §72.13.3).
	DataFrom *time.Time `json:"data_from,omitempty"`
}

// retainedFrom is the earliest statistics hour stats_retention_days keeps
// when that is after from, nil otherwise.
func (p *Plugin) retainedFrom(now, from time.Time) *time.Time {
	keep := time.Duration(p.cfg.Load().StatsRetentionDays) * 24 * time.Hour
	if cut := now.Add(-keep).Truncate(time.Hour); cut.After(from) {
		return &cut
	}
	return nil
}

// costSums selects the columns of a costSumRow from costs rows.
const costSums = `coalesce(round(sum(cost) * 100000000), 0)::bigint AS units,
	coalesce(sum(priced_calls + unpriced_calls + free_calls), 0) AS calls, coalesce(sum(priced_calls), 0) AS priced_calls,
	coalesce(sum(unpriced_calls), 0) AS unpriced_calls, coalesce(sum(unpriced_tokens), 0) AS unpriced_tokens,
	coalesce(sum(free_calls), 0) AS free_calls, coalesce(sum(tokens_in), 0) AS tokens_in, coalesce(sum(tokens_out), 0) AS tokens_out`

// costSumRow scans costSums: CostSummary keeps its units unexported.
type costSumRow struct {
	Units, Calls, PricedCalls, UnpricedCalls, UnpricedTokens, FreeCalls, TokensIn, TokensOut int64
}

func (r costSumRow) summary() CostSummary {
	return CostSummary{units: r.Units, Calls: r.Calls, PricedCalls: r.PricedCalls, UnpricedCalls: r.UnpricedCalls,
		UnpricedTokens: r.UnpricedTokens, FreeCalls: r.FreeCalls, TokensIn: r.TokensIn, TokensOut: r.TokensOut}
}

// getCosts: day buckets start at the caller's midnight (tz_offset_minutes,
// like the statistics).
func (p *Plugin) getCosts(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	ck := p.clockOf(req)
	now := ck.now
	r := CostReport{Range: pluginsdk.Query(req, "range"), To: now, Bucket: "day", ByChannel: []ChannelCost{}, BySource: []SourceCost{}, Trend: []CostPoint{}}
	switch r.Range {
	case "", "24h":
		r.Range, r.Bucket, r.From = "24h", "hour", now.Add(-24*time.Hour).Truncate(time.Hour)
	case "7d":
		r.From = now.Add(-7 * 24 * time.Hour)
	case "30d":
		r.From = now.Add(-30 * 24 * time.Hour)
	default:
		return pluginsdk.FieldErrorResponse("invalid query / 查询参数无效",
			pluginsdk.FieldErrors{}.Add("range", "enum", "range must be 24h, 7d or 30d / range 取值为 24h、7d、30d")), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	if err := p.loadCostReport(ctx, db, &r, ck); err != nil {
		return nil, err
	}
	r.DataFrom = p.retainedFrom(now, r.From)
	return pluginsdk.DataResponse(r), nil
}

// loadCostReport reads the costs rows since r.From, this node's pending sums
// included (a test shows up at once); the trend is bucketed by ck.
func (p *Plugin) loadCostReport(ctx context.Context, db *gorm.DB, r *CostReport, ck clock) error {
	p.flushStats(ctx)
	costs := func() *gorm.DB { return db.Model(&model.Cost{}).Where("hour >= ?", r.From) }
	var totals costSumRow
	if err := costs().Select(costSums).Scan(&totals).Error; err != nil {
		return err
	}
	r.Totals = totals.summary()
	var channels []struct {
		ChannelID                       int64
		Name, Type, Model, BillingModel string
		Deleted                         bool
		Sum                             costSumRow `gorm:"embedded"`
	}
	err := db.Table(model.Prefix+"costs k").Select(`k.channel_id, coalesce(c.name, '') AS name, coalesce(c.type, '') AS type, coalesce(c.model, '') AS model,
			coalesce(c.billing_model, '') AS billing_model, c.id IS NULL OR c.deleted_at IS NOT NULL AS deleted, `+costSums).
		Joins("LEFT JOIN "+model.Prefix+"channels c ON c.id = k.channel_id").Where("k.hour >= ?", r.From).
		Group("k.channel_id, c.id").Order("sum(k.cost) DESC, k.channel_id").Scan(&channels).Error
	if err != nil {
		return err
	}
	for _, c := range channels {
		r.ByChannel = append(r.ByChannel, ChannelCost{ChannelID: c.ChannelID, Name: c.Name, Type: c.Type, Model: c.Model,
			BillingModel: c.BillingModel, Deleted: c.Deleted, Summary: c.Sum.summary()})
	}
	var sources []struct {
		Source string
		Sum    costSumRow `gorm:"embedded"`
	}
	if err := costs().Select("source, " + costSums).Group("source").Order("source").Scan(&sources).Error; err != nil {
		return err
	}
	for _, s := range sources {
		r.BySource = append(r.BySource, SourceCost{Source: s.Source, Summary: s.Sum.summary()})
	}
	var hours []struct {
		Hour  time.Time
		Units int64
	}
	if err := costs().Select("hour, round(sum(cost) * 100000000)::bigint AS units").Group("hour").Scan(&hours).Error; err != nil {
		return err
	}
	byBucket := map[int64]int64{}
	for _, h := range hours {
		byBucket[ck.bucket(h.Hour, r.Bucket).Unix()] += h.Units
	}
	step := 24 * time.Hour
	if r.Bucket == "hour" {
		step = time.Hour
	}
	for b := ck.bucket(r.From.Truncate(time.Hour), r.Bucket); !b.After(r.To); b = b.Add(step) {
		r.Trend = append(r.Trend, CostPoint{Bucket: b, Cost: formatCost(byBucket[b.Unix()])})
	}
	return nil
}

// channelCosts30d sums the last 30 days of each channel, this node's pending
// sums included.
func (p *Plugin) channelCosts30d(ctx context.Context, db *gorm.DB, since time.Time, channels []int64) (map[int64]CostSummary, error) {
	p.flushStats(ctx)
	var rows []struct {
		ChannelID int64
		Sum       costSumRow `gorm:"embedded"`
	}
	err := db.Model(&model.Cost{}).Select("channel_id, "+costSums).Where("hour >= ? AND channel_id = ANY(?)", since, pgdb.Array[int64](channels)).
		Group("channel_id").Scan(&rows).Error
	out := make(map[int64]CostSummary, len(rows))
	for _, r := range rows {
		out[r.ChannelID] = r.Sum.summary()
	}
	return out, err
}

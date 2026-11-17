package usage

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/account"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/perm"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// The account side of the usage records (CONTRACTS §103.5): a holder of
// usage:account:read, at any scope, reads the requests the accounts of its
// scope served. Its answers are types of their own that list the fields it
// may see (D1), so a field added to Record does not reach it.

// AccountRecord is the account side's view of a usage_logs row; its fields
// are named as in Record.
type AccountRecord struct {
	ID                    int64           `json:"id"`
	RequestID             string          `json:"request_id"`
	CreatedAt             time.Time       `json:"created_at"`
	AccountID             *int64          `json:"account_id"`
	AccountName           string          `json:"account_name"`
	PluginKey             string          `json:"plugin_key"`
	Platform              string          `json:"platform"`
	AccountType           string          `json:"account_type"`
	Protocol              string          `json:"protocol"`
	Endpoint              string          `json:"endpoint"`
	Model                 string          `json:"model"`
	UpstreamModel         string          `json:"upstream_model"`
	Stream                bool            `json:"stream"`
	StatusCode            int             `json:"status_code"`
	Success               bool            `json:"success"`
	ErrorType             string          `json:"error_type"`
	Attempts              int             `json:"attempts"`
	InputTokens           int64           `json:"input_tokens"`
	OutputTokens          int64           `json:"output_tokens"`
	CacheReadTokens       int64           `json:"cache_read_tokens"`
	CacheCreationTokens   int64           `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64           `json:"cache_creation_1h_tokens"`
	StandardCost          decimal.Decimal `json:"standard_cost"`
	LatencyMs             int             `json:"latency_ms"`
	FirstTokenMs          int             `json:"first_token_ms"`
	// AttemptTrail is shaped trailScoped.
	AttemptTrail json.RawMessage `json:"attempt_trail"`
}

// AccountDetail is the account side's view of one record.
type AccountDetail struct {
	AccountRecord
	Metrics json.RawMessage `json:"metrics"`
}

// AccountTotals is Totals without the charge.
type AccountTotals struct {
	Requests            int64           `json:"requests"`
	Success             int64           `json:"success"`
	InputTokens         int64           `json:"input_tokens"`
	OutputTokens        int64           `json:"output_tokens"`
	CacheReadTokens     int64           `json:"cache_read_tokens"`
	CacheCreationTokens int64           `json:"cache_creation_tokens"`
	PromptTokens        *int64          `json:"prompt_tokens"`
	StandardCost        decimal.Decimal `json:"standard_cost"`
	LatencyMsAvg        int64           `json:"latency_ms_avg"`
	FirstTokenMsAvg     int64           `json:"first_token_ms_avg"`
}

// AccountSummaryRow is SummaryRow without the user and the charge;
// group_by=account keys it by account id and names the account.
type AccountSummaryRow struct {
	Key         string `json:"key"`
	AccountName string `json:"account_name,omitempty"`
	AccountTotals
	TotalTokens int64 `json:"total_tokens"`
	Anomalies   int64 `json:"anomalies"`
	Estimated   int64 `json:"estimated"`
}

// AccountOverview is Overview over the accounts of the scope: no charge, no
// API keys or users, and the accounts the scope reaches.
type AccountOverview struct {
	TZ        string        `json:"tz"`
	Today     AccountTotals `json:"today"`
	Yesterday AccountTotals `json:"yesterday"`
	Last30d   AccountTotals `json:"last_30d"`
	Last5m    struct {
		Requests int64 `json:"requests"`
		Tokens   int64 `json:"tokens"`
	} `json:"last_5m"`
	Accounts account.Counts `json:"accounts"`
}

// accountParams are the query parameters of the account side's filter;
// every other one is refused (accountFilter).
var accountParams = []string{"account_id", "model", "platform", "plugin_key", "account_type", "protocol", "endpoint",
	"error_type", "request_id", "success", "stream", "status_code", "retried", "from", "to", "tz"}

// accountGroups are the account side's group_by values.
var accountGroups = groupBys{"day", "hour", "none", "model", "account", "platform", "endpoint", "protocol"}

// onlyParams refuses a query parameter of c outside allowed, each one named
// (not_allowed).
func onlyParams(c *gin.Context, allowed ...[]string) error {
	var refused []core.FieldError
	for name := range c.Request.URL.Query() {
		if !slices.ContainsFunc(allowed, func(names []string) bool { return slices.Contains(names, name) }) {
			refused = append(refused, core.FieldError{Field: name, Code: "not_allowed", Message: name + " is not a filter of the account usage"})
		}
	}
	if len(refused) == 0 {
		return nil
	}
	slices.SortFunc(refused, func(a, b core.FieldError) int { return cmp.Compare(a.Field, b.Field) })
	return core.InvalidFields(refused...)
}

// inScope keeps the rows served by an account scope reaches, deleted
// accounts included (their history stays readable); a row without an
// account is in no scope. db builds the subquery.
func inScope(f *store.Conds, db *gorm.DB, scope core.DataScope) {
	if scope.All {
		f.And("u.account_id IS NOT NULL")
		return
	}
	f.And("u.account_id IN (?)", db.Table("accounts a").Select("a.id").Scopes(core.AccountRows().Scope(scope)))
}

// accountFilter is the caller's usage:account:read scope and the filter of
// the account side: the rows of the scope, narrowed by accountParams; the
// route's own parameters are extra.
func (s *Service) accountFilter(c *gin.Context, extra ...string) (core.DataScope, store.Conds, error) {
	ctx := c.Request.Context()
	scope, err := core.Scope(ctx, s.Authz, perm.UsageAccountRead)
	if err != nil {
		return core.DataScope{}, nil, err
	}
	if err := onlyParams(c, accountParams, extra); err != nil {
		return core.DataScope{}, nil, err
	}
	f, err := parseFilter(c, nil, trailScoped)
	if err != nil {
		return core.DataScope{}, nil, err
	}
	inScope(&f, store.G(ctx, s.db.Gorm), scope)
	return scope, f, nil
}

// shapeScoped shapes the attempt trails of rs for the account side: one
// query reads which of their accounts the scope reaches, and their names.
// An error fails the answer rather than show the accounts unchecked.
func (s *Service) shapeScoped(ctx context.Context, scope core.DataScope, rs []Record) error {
	var ids []int64
	for _, r := range rs {
		for _, id := range gjson.GetBytes(r.AttemptTrail, "steps.#.account_id").Array() {
			ids = append(ids, id.Int())
		}
	}
	names := map[int64]string{}
	if len(ids) > 0 {
		var rows []struct {
			ID   int64
			Name string
		}
		if err := store.G(ctx, s.db.Gorm).Table("accounts a").Select("a.id, a.name").
			Where("a.id = ANY(?)", store.Int64s(ids)).Scopes(core.AccountRows().Scope(scope)).Scan(&rows).Error; err != nil {
			return err
		}
		for _, a := range rows {
			names[a.ID] = a.Name
		}
	}
	for i := range rs {
		rs[i].shapeTrail(trailScoped, names)
	}
	return nil
}

func accountRecord(r *Record) AccountRecord {
	return AccountRecord{ID: r.ID, RequestID: r.RequestID, CreatedAt: r.CreatedAt, AccountID: r.AccountID, AccountName: r.AccountName,
		PluginKey: r.PluginKey, Platform: r.Platform, AccountType: r.AccountType, Protocol: r.Protocol, Endpoint: r.Endpoint,
		Model: r.Model, UpstreamModel: r.UpstreamModel, Stream: r.Stream, StatusCode: r.StatusCode, Success: r.Success,
		ErrorType: r.ErrorType, Attempts: r.Attempts, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
		CacheReadTokens: r.CacheReadTokens, CacheCreationTokens: r.CacheCreationTokens, CacheCreation1hTokens: r.CacheCreation1hTokens,
		StandardCost: r.StandardCost, LatencyMs: r.LatencyMs, FirstTokenMs: r.FirstTokenMs, AttemptTrail: r.AttemptTrail}
}

func accountTotals(t Totals) AccountTotals {
	return AccountTotals{Requests: t.Requests, Success: t.Success, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
		CacheReadTokens: t.CacheReadTokens, CacheCreationTokens: t.CacheCreationTokens, PromptTokens: t.PromptTokens,
		StandardCost: t.StandardCost, LatencyMsAvg: t.LatencyMsAvg, FirstTokenMsAvg: t.FirstTokenMsAvg}
}

// GET /account-usage: the list, paged as /usage (page and page_size, or
// cursor).
func (s *Service) accountUsage(c *gin.Context) {
	scope, f, err := s.accountFilter(c, "page", "page_size", "cursor")
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.servePage(c, f, func(rs []Record) (any, error) {
		if err := s.shapeScoped(c.Request.Context(), scope, rs); err != nil {
			return nil, err
		}
		out := make([]AccountRecord, len(rs))
		for i := range rs {
			out[i] = accountRecord(&rs[i])
		}
		return out, nil
	})
}

// GET /account-usage/:id: a record of the scope, else 404.
func (s *Service) accountUsageDetail(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := httpapi.PathID(c, "id")
	if !ok {
		return
	}
	scope, err := core.Scope(ctx, s.Authz, perm.UsageAccountRead)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	g := store.G(ctx, s.db.Gorm)
	var f store.Conds
	f.And("u.id = ?", id)
	inScope(&f, g, scope)
	var d AccountDetail
	rs := make([]Record, 1)
	err = g.Table("usage_logs u").Select(recordColumns + `, u.metrics`).Joins(recordJoins).Scopes(f.Scope()).Row().
		Scan(append(rs[0].scanTargets(), (*store.Bytes)(&d.Metrics))...)
	if store.IsNoRows(err) {
		err = core.ErrNotFound.WithMessage("usage record not found")
	}
	if err == nil {
		err = s.shapeScoped(ctx, scope, rs)
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	d.AccountRecord = accountRecord(&rs[0])
	httpapi.OK(c, d)
}

// GET /account-usage/summary: /usage/summary over the scope, by
// accountGroups, ranked by requests or tokens (order=cost would rank by the
// charge). Cached like an operator's, per scope.
func (s *Service) accountSummary(c *gin.Context) {
	scope, f, err := s.accountFilter(c, "group_by", "order", "limit")
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	order := c.Query("order")
	switch order {
	case "":
		if c.Query("limit") != "" {
			order = "requests"
		}
	case "requests", "tokens":
	default:
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("order must be requests or tokens"))
		return
	}
	rows, err := s.summaryRows(c, f, accountGroups, order, "account|"+scopeKey(scope)+"|"+c.Request.URL.Query().Encode())
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	out := make([]AccountSummaryRow, len(rows))
	for i, r := range rows {
		out[i] = AccountSummaryRow{Key: r.Key, AccountName: r.Extra,
			AccountTotals: AccountTotals{Requests: r.Requests, Success: r.Success, InputTokens: r.InputTokens,
				OutputTokens: r.OutputTokens, CacheReadTokens: r.CacheReadTokens, CacheCreationTokens: r.CacheCreationTokens,
				PromptTokens: r.PromptTokens, StandardCost: r.StandardCost, LatencyMsAvg: r.LatencyMsAvg, FirstTokenMsAvg: r.FirstTokenMsAvg},
			TotalTokens: r.TotalTokens, Anomalies: r.Anomalies, Estimated: r.Estimated}
	}
	httpapi.OK(c, out)
}

// GET /account-usage/overview?tz=: /usage/overview over the scope, cached
// per scope.
func (s *Service) accountOverview(c *gin.Context) {
	ctx := c.Request.Context()
	scope, err := core.Scope(ctx, s.Authz, perm.UsageAccountRead)
	if err == nil {
		err = onlyParams(c, []string{"tz"})
	}
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	tz, loc, err := parseTZ(c)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	o, hit, err := s.overviews.get(ctx, "account|"+tz+"|"+scopeKey(scope), func(ctx context.Context) (Overview, error) {
		return s.loadOverview(ctx, tz, loc, overviewOf{accounts: scope, scoped: true})
	})
	c.Header("X-Snapshot-Cache", snapshotHeader(hit))
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	out := AccountOverview{TZ: o.TZ, Today: accountTotals(o.Today), Yesterday: accountTotals(o.Yesterday), Last30d: accountTotals(o.Last30d), Last5m: o.Last5m}
	if o.Accounts != nil {
		out.Accounts = *o.Accounts
	}
	httpapi.OK(c, out)
}

// GET /account-usage/realtime: /usage/realtime over the scope.
func (s *Service) accountRealtime(c *gin.Context) {
	_, f, err := s.accountFilter(c)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	s.serveRealtime(c, f)
}

// GET /account-usage/export: the account side's columns (accountExportColumns).
func (s *Service) accountExport(c *gin.Context) {
	scope, f, err := s.accountFilter(c, "lang")
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	cols := slices.DeleteFunc(slices.Clone(exportColumns), func(col exportColumn) bool {
		return !slices.Contains(accountExportColumns, col.en)
	})
	s.streamExport(c, csvExport{
		filter: func() (store.Conds, error) { return slices.Clone(f), nil },
		cols:   cols,
		shape:  func(rs []Record) error { return s.shapeScoped(c.Request.Context(), scope, rs) },
		audit:  map[string]any{"view": "account"},
	})
}

package moderation

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// ---------------------------------------------------------------- channels

// ChannelView is a channel as the API returns it: never the key or proxy.
type ChannelView struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Protocol   string `json:"protocol"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	APIKeyTail string `json:"api_key_tail"`
	HasKey     bool   `json:"has_key"`
	HasProxy   bool   `json:"has_proxy"`
	Proxy      string `json:"proxy"` // password redacted
	Model      string `json:"model"`
	// BillingModel is the model the calls are priced on ("" = model).
	BillingModel string    `json:"billing_model"`
	TimeoutMs    int       `json:"timeout_ms"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// List and detail only.
	PriceState *PriceState  `json:"price_state,omitempty"`
	Cost30d    *CostSummary `json:"cost_30d,omitempty"`
	// Cost30dFrom: stats_retention_days is under 30, cost_30d starts here.
	Cost30dFrom *time.Time   `json:"cost_30d_from,omitempty"`
	Stats       *ChannelStat `json:"stats,omitempty"`
	// UsedBy lists the auditors on the channel.
	UsedBy []AuditorRef `json:"used_by,omitempty"`
}

// AuditorRef names an auditor.
type AuditorRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// channelColumns are the columns of a ChannelView: the sealed key and proxy
// only as whether they are set.
const channelColumns = `id, type, protocol, name, base_url, api_key_tail, api_key_enc IS NOT NULL AS has_key,
	proxy_enc IS NOT NULL AS has_proxy, model, billing_model, timeout_ms, enabled, created_at, updated_at`

// channelViewRow is a channel row as channelColumns selects it.
type channelViewRow struct {
	ID                                        int64
	Type, Protocol, Name, BaseURL, APIKeyTail string
	HasKey, HasProxy                          bool
	Model, BillingModel                       string
	TimeoutMs                                 int
	Enabled                                   bool
	CreatedAt, UpdatedAt                      time.Time
}

// channelViews reads the channels q selects, by id.
func channelViews(q *gorm.DB) ([]ChannelView, error) {
	var rows []channelViewRow
	if err := q.Model(&model.Channel{}).Select(channelColumns).Order("id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ChannelView, len(rows))
	for i, r := range rows {
		out[i] = ChannelView{ID: r.ID, Type: r.Type, Protocol: r.Protocol, Name: r.Name, BaseURL: r.BaseURL, APIKeyTail: r.APIKeyTail,
			HasKey: r.HasKey, HasProxy: r.HasProxy, Model: r.Model, BillingModel: r.BillingModel, TimeoutMs: r.TimeoutMs,
			Enabled: r.Enabled, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	}
	return out, nil
}

// takeChannel reads the one channel q selects (gorm.ErrRecordNotFound: none).
func takeChannel(q *gorm.DB) (ChannelView, error) {
	list, err := channelViews(q)
	if err == nil && len(list) == 0 {
		err = gorm.ErrRecordNotFound
	}
	if err != nil {
		return ChannelView{}, err
	}
	return list[0], nil
}

// insertRow creates a row of m from the columns of row and returns its id.
// A map, not a model: GORM would write a column's default for a zero field
// (a disabled channel or flow would be enabled).
func insertRow(tx *gorm.DB, m any, row map[string]any) (int64, error) {
	err := tx.Model(m).Clauses(clause.Returning{Columns: columns("id")}).Create(row).Error
	id, _ := row["id"].(int64)
	return id, err
}

// channelInput is the POST / PUT body. api_key absent or "" keeps the stored
// key (PUT); type cannot change.
type channelInput struct {
	Type     string  `json:"type"`
	Protocol string  `json:"protocol"`
	Name     string  `json:"name"`
	BaseURL  string  `json:"base_url"`
	APIKey   *string `json:"api_key"`
	Model    string  `json:"model"`
	// BillingModel: the price row the calls are priced on ("" = model).
	BillingModel string `json:"billing_model"`
	TimeoutMs    int    `json:"timeout_ms"`
	Enabled      *bool  `json:"enabled"`
	// Proxy: absent keeps it, "" removes it.
	Proxy *string `json:"proxy"`
}

// row is the channels row of a new channel.
func (in *channelInput) row(enabled bool, now time.Time) map[string]any {
	return map[string]any{"type": in.Type, "protocol": in.Protocol, "name": in.Name, "base_url": in.BaseURL, "model": in.Model,
		"billing_model": in.BillingModel, "timeout_ms": in.TimeoutMs, "enabled": enabled, "created_at": now, "updated_at": now}
}

func (in *channelInput) normalize() pluginsdk.FieldErrors {
	var errs pluginsdk.FieldErrors
	in.Name, in.BaseURL, in.Model = strings.TrimSpace(in.Name), strings.TrimSpace(in.BaseURL), strings.TrimSpace(in.Model)
	in.BillingModel = strings.TrimSpace(in.BillingModel)
	if _, ok := defaultChannelTimeoutMs[in.Type]; !ok {
		errs = errs.Add("type", "enum", "type must be openai_moderation, typesafe or llm / type 取值为 openai_moderation、typesafe、llm")
	}
	if in.Type == ChannelLLM {
		if in.Protocol == "" {
			in.Protocol = ProtocolOpenAIChat
		}
		if llmProtocols[in.Protocol] == nil {
			errs = errs.Add("protocol", "enum", "protocol must be openai_chat, openai_responses or anthropic / 协议取值为 openai_chat、openai_responses、anthropic")
		}
	} else {
		in.Protocol = ""
	}
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		errs = errs.Add("name", "invalid", "name is required, at most 64 characters / 名称必填，最多 64 字")
	}
	if !baseURLRe.MatchString(in.BaseURL) || len(in.BaseURL) > 500 {
		errs = errs.Add("base_url", "invalid", "base_url must be an http(s) URL / base_url 必须是 http(s) 地址")
	}
	if in.Model == "" {
		in.Model = defaultChannelModel[in.Type]
	}
	if in.Model == "" || utf8.RuneCountInString(in.Model) > 200 {
		errs = errs.Add("model", "invalid", "model is required, at most 200 characters / 模型必填，最多 200 字")
	}
	if utf8.RuneCountInString(in.BillingModel) > 200 {
		errs = errs.Add("billing_model", "too_long", "billing_model is at most 200 characters / 计价模型最多 200 字")
	}
	if in.TimeoutMs == 0 {
		in.TimeoutMs = defaultChannelTimeoutMs[in.Type]
	}
	if in.TimeoutMs < 500 || in.TimeoutMs > 60000 {
		errs = errs.Add("timeout_ms", "out_of_range", "timeout_ms must be between 500 and 60000 / timeout_ms 取值 500–60000")
	}
	if in.APIKey != nil && len(*in.APIKey) > 4000 {
		errs = errs.Add("api_key", "too_long", "api_key is too long / api_key 过长")
	}
	if in.Proxy != nil {
		if _, err := parseProxy(*in.Proxy); err != nil {
			errs = errs.Add("proxy", "invalid", "proxy must be an http, https, socks5 or socks5h URL / 代理必须是 http、https、socks5 或 socks5h 地址")
		}
	}
	return errs
}

func (p *Plugin) listChannels(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := channelViews(db.Where("deleted_at IS NULL"))
	if err != nil {
		return nil, err
	}
	if err := p.decorateChannels(ctx, req, db, list); err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(list), nil
}

// decorateChannels adds the price state, the 30-day cost, the health and
// cost columns and the auditors using each channel of list.
func (p *Plugin) decorateChannels(ctx context.Context, req *pluginv1.HTTPRequest, db *gorm.DB, list []ChannelView) error {
	ids := make([]int64, len(list))
	for i, c := range list {
		ids[i] = c.ID
	}
	costs, err := p.channelCosts30d(ctx, db, p.now().UTC().Add(-30*24*time.Hour), ids)
	if err != nil {
		return err
	}
	stats, err := p.channelStats(ctx, db, p.clockOf(req), ids)
	if err != nil {
		return err
	}
	proxies, err := p.redactedProxies(ctx, db, ids)
	if err != nil {
		return err
	}
	var auditors []model.Auditor
	err = db.Select("channel_id, id, name").Where("deleted_at IS NULL AND channel_id = ANY(?)", pgdb.Array[int64](ids)).
		Order("id").Find(&auditors).Error
	if err != nil {
		return err
	}
	users := map[int64][]AuditorRef{}
	for _, a := range auditors {
		users[a.ChannelID] = append(users[a.ChannelID], AuditorRef{ID: a.ID, Name: a.Name})
	}
	for i := range list {
		c := &list[i]
		p.decorateChannel(ctx, c, costs)
		c.Stats = cmp.Or(stats[c.ID], newChannelStat())
		c.UsedBy, c.Proxy = users[c.ID], proxies[c.ID]
	}
	return nil
}

// redactedProxies maps the channels of ids that have a proxy to its
// redacted URL (empty when it does not open).
func (p *Plugin) redactedProxies(ctx context.Context, db *gorm.DB, ids []int64) (map[int64]string, error) {
	var rows []model.Channel
	err := db.Select("id, proxy_enc").Where("proxy_enc IS NOT NULL AND deleted_at IS NULL AND id = ANY(?)", pgdb.Array[int64](ids)).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, r := range rows {
		proxy, err := p.openProxy(ctx, r.ID, r.ProxyEnc)
		if err != nil {
			p.log.Warn("moderation: channel proxy does not open", "channel_id", r.ID, "error", err.Error())
			continue
		}
		out[r.ID] = redactProxy(proxy)
	}
	return out, nil
}

// decorateChannel adds the price state and the 30-day cost.
func (p *Plugin) decorateChannel(ctx context.Context, c *ChannelView, costs map[int64]CostSummary) {
	model := c.BillingModel
	if model == "" {
		model = c.Model
	}
	st := p.priceState(ctx, c.Type, model)
	c.PriceState = &st
	sum := costs[c.ID]
	c.Cost30d = &sum
	now := p.now().UTC()
	c.Cost30dFrom = p.retainedFrom(now, now.Add(-30*24*time.Hour))
}

func (p *Plugin) getChannel(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	c, err := takeChannel(db.Where("id = ? AND deleted_at IS NULL", id))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return notFound("channel"), nil
	}
	if err != nil {
		return nil, err
	}
	list := []ChannelView{c}
	if err := p.decorateChannels(ctx, req, db, list); err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(list[0]), nil
}

// errorWithDetails is the SDK error envelope with details.
func errorWithDetails(status int, code, message string, details map[string]any) *pluginv1.HTTPResponse {
	return pluginsdk.JSONResponse(status, map[string]any{"error": map[string]any{"code": code, "message": message, "details": details}})
}

func notFound(what string) *pluginv1.HTTPResponse {
	zh := map[string]string{"channel": "渠道", "auditor": "审核器", "flow": "流程", "event": "记录"}[what]
	return pluginsdk.ErrorResponse(http.StatusNotFound, "not_found", what+" not found / "+zh+"不存在")
}

func (p *Plugin) createChannel(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in channelInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	if errs := in.normalize(); len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid channel / 渠道参数无效", errs), nil
	}
	enabled := in.Enabled == nil || *in.Enabled
	var out ChannelView
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		now := p.now().UTC()
		id, err := insertRow(tx, &model.Channel{}, in.row(enabled, now))
		if err != nil {
			return err
		}
		if err := p.storeKey(ctx, tx, id, in.APIKey); err != nil {
			return err
		}
		if err := p.storeProxy(ctx, tx, id, in.Proxy); err != nil {
			return err
		}
		out, err = takeChannel(tx.Where("id = ?", id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return p.getChannel(ctx, withParam(req, "id", out.ID))
}

// storeKey seals and stores the key when one is given.
func (p *Plugin) storeKey(ctx context.Context, tx *gorm.DB, id int64, key *string) error {
	if key == nil || strings.TrimSpace(*key) == "" {
		return nil
	}
	k := strings.TrimSpace(*key)
	enc, err := p.host.SealSecret(ctx, secretContext(id, "key"), []byte(k))
	if err != nil {
		return fmt.Errorf("seal channel key: %w", err)
	}
	return tx.Model(&model.Channel{}).Where("id = ?", id).Updates(map[string]any{"api_key_enc": enc, "api_key_tail": keyTail(k)}).Error
}

// keyTail is the last 4 characters shown for a key.
func keyTail(k string) string {
	r := []rune(k)
	if len(r) <= 4 {
		return ""
	}
	return string(r[len(r)-4:])
}

func (p *Plugin) updateChannel(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var in channelInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	var out ChannelView
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		cur, err := takeChannel(tx.Clauses(forUpdate()).Where("id = ? AND deleted_at IS NULL", id))
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp = notFound("channel")
			return errRollback
		}
		if err != nil {
			return err
		}
		if in.Type == "" {
			in.Type = cur.Type
		}
		errs := in.normalize()
		if in.Type != cur.Type {
			errs = errs.Add("type", "immutable", "type cannot change / 渠道类型不能修改")
		}
		newKey := in.APIKey != nil && strings.TrimSpace(*in.APIKey) != ""
		var proxyChanged bool
		if in.Proxy, proxyChanged, err = p.proxyChange(ctx, tx, id, in.Proxy); err != nil {
			return err
		}
		if !newKey && cur.HasKey && (hostChanged(cur.BaseURL, in.BaseURL) || proxyChanged) {
			// new-api channel.go: a key must not follow the channel to another
			// host or through another proxy.
			errs = errs.Add("api_key", "required", "changing the base_url host or the proxy needs the key again / 修改 base_url 地址或代理需要重新填写 key")
		}
		if len(errs) > 0 {
			resp = pluginsdk.FieldErrorResponse("invalid channel / 渠道参数无效", errs)
			return errRollback
		}
		enabled := cur.Enabled
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		err = tx.Model(&model.Channel{}).Where("id = ?", id).Updates(map[string]any{"protocol": in.Protocol, "name": in.Name,
			"base_url": in.BaseURL, "model": in.Model, "billing_model": in.BillingModel, "timeout_ms": in.TimeoutMs,
			"enabled": enabled, "updated_at": p.now().UTC()}).Error
		if err != nil {
			return err
		}
		if err := p.storeKey(ctx, tx, id, in.APIKey); err != nil {
			return err
		}
		if err := p.storeProxy(ctx, tx, id, in.Proxy); err != nil {
			return err
		}
		out, err = takeChannel(tx.Where("id = ?", id))
		return err
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getChannel(ctx, withParam(req, "id", out.ID))
}

// proxyChange reads what a PUT does to the stored proxy: sent comes back nil
// when it is the redacted proxy the console was shown (the stored one is
// kept); changed = the calls (and the key) would go through another proxy.
func (p *Plugin) proxyChange(ctx context.Context, tx *gorm.DB, id int64, sent *string) (_ *string, changed bool, _ error) {
	if sent == nil {
		return nil, false, nil
	}
	var ch model.Channel
	if err := tx.Select("proxy_enc").Where("id = ?", id).Take(&ch).Error; err != nil {
		return nil, false, err
	}
	cur, err := p.openProxy(ctx, id, ch.ProxyEnc)
	v := strings.TrimSpace(*sent)
	if err == nil && v == redactProxy(cur) {
		return nil, false, nil
	}
	return sent, err != nil || cur != v, nil
}

// hostChanged compares scheme and host of two URLs.
func hostChanged(a, b string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSpace(u))
		scheme, rest, _ := strings.Cut(u, "://")
		host, _, _ := strings.Cut(rest, "/")
		return scheme + "://" + host
	}
	return norm(a) != norm(b)
}

func (p *Plugin) deleteChannel(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		var used []AuditorRef
		if err := tx.Model(&model.Auditor{}).Select("id, name").Where("channel_id = ? AND deleted_at IS NULL", id).Order("id").Scan(&used).Error; err != nil {
			return err
		}
		if len(used) > 0 {
			resp = errorWithDetails(http.StatusConflict, "in_use", "the channel is used by auditors / 该渠道仍被审核器使用", map[string]any{"auditors": used})
			return errRollback
		}
		res := tx.Model(&model.Channel{}).Where("id = ? AND deleted_at IS NULL", id).
			Updates(map[string]any{"deleted_at": p.now().UTC(), "api_key_enc": nil, "proxy_enc": nil})
		if res.Error == nil && res.RowsAffected == 0 {
			resp = notFound("channel")
			return errRollback
		}
		return res.Error
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(map[string]any{"id": id, "deleted": true}), nil
}

// ChannelTestResult is POST /channels/:id/test.
type ChannelTestResult struct {
	OK        bool      `json:"ok"`
	Status    int       `json:"status,omitempty"`
	Error     string    `json:"error,omitempty"`
	Message   string    `json:"message,omitempty"` // upstream error text (key removed)
	Model     string    `json:"model,omitempty"`
	URL       string    `json:"url"` // the endpoint called
	LatencyMs int64     `json:"latency_ms"`
	Cost      *CallCost `json:"cost,omitempty"`
}

func (p *Plugin) testChannel(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	ch, err := p.loadChannel(ctx, id)
	if errors.Is(err, errNotFound) {
		return notFound("channel"), nil
	}
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, ch.Timeout)
	defer cancel()
	start := p.now()
	out := ChannelTestResult{URL: ch.endpoint()}
	var usage EngineUsage
	switch ch.Type {
	case ChannelOpenAI:
		_, out.Model, err = callModerations(cctx, ch, "hello", "")
	case ChannelTypeSafe:
		qs, _ := DefaultQuestions(nil)
		qs.Items = qs.Items[:1]
		qs.wired = qs.wire()
		var r systemOneAnswers
		r, err = callSystemOne(cctx, ch, &qs, "hello", nil)
		out.Model = r.Model
		usage = EngineUsage{InputTokens: r.Usage.InputTokens}
	case ChannelLLM:
		var t llmTurn
		t, err = protocolOf(ch).call(cctx, ch, llmRequest{Messages: []any{llmUserMessage("ping")}, MaxTokens: 16})
		out.Model = t.Model
		usage = engineUsage(t.Usage)
	}
	out.LatencyMs = p.now().Sub(start).Milliseconds()
	out.Cost = p.priceNow(ctx, SourceTest, ch, EngineResult{Usage: usage, Calls: 1}, err)
	out.OK = err == nil
	if err != nil {
		out.Error, out.Status = ErrType(err), upstreamStatus(err)
		var up *UpstreamError
		if errors.As(err, &up) {
			out.Message = up.message
		} else {
			out.Message = truncateRunes(err.Error(), 300)
		}
	}
	return pluginsdk.DataResponse(out), nil
}

// loadChannel reads one channel with its key opened (disabled ones too).
func (p *Plugin) loadChannel(ctx context.Context, id int64) (*liveChannel, error) {
	db, err := p.db(ctx)
	if err != nil {
		return nil, err
	}
	ch := &liveChannel{ID: id, client: p.httpClient, ttft: p.ttftTimeout}
	var row model.Channel
	err = db.Select("type, protocol, base_url, api_key_enc, proxy_enc, model, billing_model, timeout_ms").
		Where("id = ? AND deleted_at IS NULL", id).Take(&row).Error
	ch.Type, ch.Protocol, ch.BaseURL, ch.Model, ch.BillingModel = row.Type, row.Protocol, row.BaseURL, row.Model, row.BillingModel
	keyEnc, proxyEnc, timeout := row.APIKeyEnc, row.ProxyEnc, row.TimeoutMs
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(keyEnc) > 0 {
		k, err := p.host.OpenSecret(ctx, secretContext(id, "key"), keyEnc)
		if err != nil {
			return nil, fmt.Errorf("open channel key: %w", err)
		}
		ch.Key = string(k)
	}
	if ch.client, _, err = p.channelClient(ctx, id, proxyEnc); err != nil {
		return nil, err
	}
	ch.Timeout = time.Duration(timeout) * time.Millisecond
	if ch.Timeout <= 0 {
		ch.Timeout = time.Duration(defaultChannelTimeoutMs[ch.Type]) * time.Millisecond
	}
	if ch.Type == ChannelLLM && ch.Key != "" {
		ch.markerKey = markerKey(ch.Key)
	}
	return ch, nil
}

// errRollback aborts a configTx whose response is already decided.
var errRollback = errors.New("rollback")

// configTx runs a configuration write, bumps config_version and reloads.
func (p *Plugin) configTx(ctx context.Context, fn func(*gorm.DB) error) error {
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := fn(tx); err != nil {
			return err
		}
		return bumpConfig(tx)
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	if err == nil {
		p.configChanged(ctx)
	}
	return err
}

// ---------------------------------------------------------------- auditors

// AuditorView is an auditor as the API returns it.
type AuditorView struct {
	ID           int64           `json:"id"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	ChannelID    int64           `json:"channel_id"`
	Config       json.RawMessage `json:"config"`
	RulesVersion string          `json:"rules_version,omitempty"`
	Usable       bool            `json:"usable"` // its channel is enabled and has a key
	// The channel, for the canvas and the list (empty when deleted).
	ChannelName      string        `json:"channel_name"`
	ChannelType      string        `json:"channel_type"`
	ChannelEnabled   bool          `json:"channel_enabled"`
	ChannelTimeoutMs int           `json:"channel_timeout_ms"`
	Today            *AuditorToday `json:"today,omitempty"` // list only
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type auditorInput struct {
	Name      string        `json:"name"`
	Type      string        `json:"type"`
	ChannelID int64         `json:"channel_id"`
	Config    AuditorConfig `json:"config"`
}

// auditorViewRow is an auditor row with its channel, as auditorViews reads it.
type auditorViewRow struct {
	ID                       int64
	Name, Type               string
	ChannelID                int64
	Config                   json.RawMessage
	ChannelName, ChannelType string
	ChannelEnabled           bool
	ChannelTimeoutMs         int
	CreatedAt, UpdatedAt     time.Time
}

// auditorViews reads the live auditors, or the one of id (0 = all).
func (p *Plugin) auditorViews(db *gorm.DB, id int64) ([]AuditorView, error) {
	q := db.Table("auditors a").
		Select(`a.id, a.name, a.type, a.channel_id, a.config, coalesce(c.name, '') AS channel_name, coalesce(c.type, '') AS channel_type,
			coalesce(c.enabled, false) AS channel_enabled, coalesce(c.timeout_ms, 0) AS channel_timeout_ms, a.created_at, a.updated_at`).
		Joins("LEFT JOIN channels c ON c.id = a.channel_id AND c.deleted_at IS NULL").Where("a.deleted_at IS NULL")
	if id != 0 {
		q = q.Where("a.id = ?", id)
	}
	var rows []auditorViewRow
	if err := q.Order("a.id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	list := make([]AuditorView, len(rows))
	for i, r := range rows {
		list[i] = AuditorView{ID: r.ID, Name: r.Name, Type: r.Type, ChannelID: r.ChannelID, Config: r.Config, ChannelName: r.ChannelName,
			ChannelType: r.ChannelType, ChannelEnabled: r.ChannelEnabled, ChannelTimeoutMs: r.ChannelTimeoutMs,
			CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	}
	snap := p.snap.Load()
	for i := range list {
		if la := snap.auditors[list[i].ID]; la != nil {
			list[i].Usable = la.channel != nil && la.channel.Key != ""
			if la.questions != nil {
				list[i].RulesVersion = la.questions.RulesVersion
			}
		}
	}
	return list, nil
}

func (p *Plugin) listAuditors(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := p.auditorViews(db, 0)
	if err != nil {
		return nil, err
	}
	today, err := p.auditorsToday(ctx, db, p.clockOf(req))
	if err != nil {
		return nil, err
	}
	for i := range list {
		t := today[list[i].ID]
		list[i].Today = &t
	}
	return pluginsdk.DataResponse(list), nil
}

func (p *Plugin) getAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := p.auditorViews(db, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return notFound("auditor"), nil
	}
	return pluginsdk.DataResponse(list[0]), nil
}

// validateAuditor checks the type against the bound channel and compiles the
// config.
func (p *Plugin) validateAuditor(tx *gorm.DB, in *auditorInput) pluginsdk.FieldErrors {
	var errs pluginsdk.FieldErrors
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		errs = errs.Add("name", "invalid", "name is required, at most 64 characters / 名称必填，最多 64 字")
	}
	want, ok := channelTypeOf[in.Type]
	if !ok {
		return errs.Add("type", "enum", "type must be openai_moderation, typesafe, llm_verdict or llm_questions / type 取值为 openai_moderation、typesafe、llm_verdict、llm_questions")
	}
	var chType string
	err := tx.Model(&model.Channel{}).Select("type").Where("id = ? AND deleted_at IS NULL", in.ChannelID).Take(&chType).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		errs = errs.Add("channel_id", "not_found", "channel does not exist / 渠道不存在")
	case err != nil:
		errs = errs.Add("channel_id", "unavailable", err.Error())
	case chType != want:
		errs = errs.Add("channel_id", "type_mismatch", fmt.Sprintf("the auditor needs a %s channel / 该审核器需要 %s 类型的渠道", want, want))
	}
	def := p.cfg.Load().jevDefault
	if _, err := compileAuditor(0, in.Name, in.Type, in.ChannelID, in.Config, nil, &def); err != nil {
		errs = errs.Add("config", "invalid", err.Error())
	}
	return errs
}

func (p *Plugin) createAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in auditorInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	var id int64
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		if errs := p.validateAuditor(tx, &in); len(errs) > 0 {
			resp = pluginsdk.FieldErrorResponse("invalid auditor / 审核器参数无效", errs)
			return errRollback
		}
		cfg, _ := json.Marshal(in.Config)
		now := p.now().UTC()
		var err error
		id, err = insertAuditor(tx, in.Name, in.Type, in.ChannelID, cfg, now)
		return err
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getAuditor(ctx, withParam(req, "id", id))
}

func (p *Plugin) updateAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var in auditorInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		var curType string
		err := tx.Model(&model.Auditor{}).Clauses(forUpdate()).Select("type").Where("id = ? AND deleted_at IS NULL", id).Take(&curType).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp = notFound("auditor")
			return errRollback
		}
		if err != nil {
			return err
		}
		if in.Type == "" {
			in.Type = curType
		}
		errs := p.validateAuditor(tx, &in)
		if in.Type != curType {
			errs = errs.Add("type", "immutable", "type cannot change / 审核器类型不能修改")
		}
		if len(errs) > 0 {
			resp = pluginsdk.FieldErrorResponse("invalid auditor / 审核器参数无效", errs)
			return errRollback
		}
		cfg, _ := json.Marshal(in.Config)
		return tx.Model(&model.Auditor{}).Where("id = ?", id).
			Updates(map[string]any{"name": in.Name, "channel_id": in.ChannelID, "config": json.RawMessage(cfg), "updated_at": p.now().UTC()}).Error
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getAuditor(ctx, req)
}

func (p *Plugin) deleteAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		flows, err := flowsUsing(tx, id)
		if err != nil {
			return err
		}
		if len(flows) > 0 {
			resp = pluginsdk.ErrorResponse(http.StatusConflict, "in_use", fmt.Sprintf("the auditor is used by flows %v / 该审核器仍被流程 %v 使用", flows, flows))
			return errRollback
		}
		res := tx.Model(&model.Auditor{}).Where("id = ? AND deleted_at IS NULL", id).Update("deleted_at", p.now().UTC())
		if res.Error == nil && res.RowsAffected == 0 {
			resp = notFound("auditor")
			return errRollback
		}
		return res.Error
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(map[string]any{"id": id, "deleted": true}), nil
}

// flowsUsing lists live flows whose draft or published spec names the auditor.
func flowsUsing(tx *gorm.DB, auditorID int64) ([]int64, error) {
	ids := []int64{}
	err := tx.Model(&model.Flow{}).Where(`deleted_at IS NULL AND (
		jsonb_path_exists(draft, ?::jsonpath, jsonb_build_object('a', ?::bigint)) OR
		jsonb_path_exists(coalesce(published, '{}'), ?::jsonpath, jsonb_build_object('a', ?::bigint)))`,
		usesAuditor, auditorID, usesAuditor, auditorID).Order("id").Pluck("id", &ids).Error
	return ids, err
}

// usesAuditor is the JSON path of a spec's auditors equal to $a. It is bound
// as a parameter: written into the statement, GORM would read its ? as a
// placeholder and its @ as a named argument.
const usesAuditor = `$.stages[*].auditors[*] ? (@ == $a)`

// insertAuditor creates an auditor and returns its id.
func insertAuditor(tx *gorm.DB, name, typ string, channel int64, cfg []byte, now time.Time) (int64, error) {
	row := model.Auditor{Name: name, Type: typ, ChannelID: channel, Config: cfg, CreatedAt: now, UpdatedAt: now}
	err := tx.Clauses(clause.Returning{Columns: columns("id")}).Create(&row).Error
	return row.ID, err
}

// TryResult is POST /auditors/:id/try.
type TryResult struct {
	Outcome
	Error string `json:"error,omitempty"`
}

func (p *Plugin) tryAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	return p.tryWith(ctx, req, id)
}

// tryDraftAuditor tries an auditor that is not saved yet (POST /auditors/try).
func (p *Plugin) tryDraftAuditor(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	return p.tryWith(ctx, req, 0)
}

// auditorDraft is the unsaved part of a try: each field set overrides the
// stored auditor.
type auditorDraft struct {
	Name      *string        `json:"name"`
	Type      *string        `json:"type"`
	ChannelID *int64         `json:"channel_id"`
	Config    *AuditorConfig `json:"config"`
}

func (d *auditorDraft) apply(src *auditorSource) {
	if d.Name != nil {
		src.name = *d.Name
	}
	if d.Type != nil {
		src.typ = *d.Type
	}
	if d.ChannelID != nil {
		src.channel = *d.ChannelID
	}
	if d.Config != nil {
		src.cfg = *d.Config
	}
}

// tryWith runs one auditor on {text, images?, draft?}. draft {name, type,
// channel_id, config} overrides the saved auditor id (all of it when id is
// 0), so unsaved changes can be tried (new-api TryModerationAuditor).
func (p *Plugin) tryWith(ctx context.Context, req *pluginv1.HTTPRequest, id int64) (*pluginv1.HTTPResponse, error) {
	var body struct {
		testBody
		Draft json.RawMessage `json:"draft"`
	}
	if err := json.Unmarshal(req.GetBody(), &body); err != nil {
		return invalidJSON(), nil
	}
	in, bad := body.input(p.cfg.Load())
	if bad != nil {
		return bad, nil
	}
	var draft *auditorDraft
	if len(body.Draft) > 0 && string(body.Draft) != "null" {
		draft = &auditorDraft{}
		if err := json.Unmarshal(body.Draft, draft); err != nil {
			return tryError("draft", "invalid", "draft is not a valid auditor / 审核器草稿格式无效"), nil
		}
	}
	if id == 0 && draft == nil {
		return tryError("draft", "required", "draft is required / 请提供审核器草稿"), nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	a, err := p.tryAuditorOf(ctx, db, id, draft)
	var ce *compileError
	switch {
	case errors.Is(err, errNotFound):
		return notFound("auditor"), nil
	case errors.Is(err, errStoredAuditor):
		return pluginsdk.ErrorResponse(http.StatusConflict, "invalid_config", "stored auditor config is invalid / 审核器配置无效"), nil
	case errors.As(err, &ce):
		return tryError(ce.field, ce.code, ce.msg), nil
	case err != nil:
		return nil, err
	}
	o := p.audit(ctx, a, in, nil, true, SourceTry)
	out := TryResult{Outcome: o}
	if o.Err != nil {
		out.ErrType, out.Status, out.Error = ErrType(o.Err), upstreamStatus(o.Err), truncateRunes(o.Err.Error(), 300)
	}
	return pluginsdk.DataResponse(out), nil
}

func tryError(field, code, msg string) *pluginv1.HTTPResponse {
	return pluginsdk.FieldErrorResponse("invalid try / 试跑参数无效", pluginsdk.FieldErrors{}.Add(field, code, msg))
}

// tryAuditorOf compiles what a try runs: the stored auditor, a draft over
// it, or a draft alone (id 0). A stored auditor that does not compile is
// errStoredAuditor; a draft that does not, a *compileError.
func (p *Plugin) tryAuditorOf(ctx context.Context, db *gorm.DB, id int64, d *auditorDraft) (*liveAuditor, error) {
	src := auditorSource{name: "draft"}
	if id != 0 {
		var err error
		if src, err = auditorSourceOf(db, id); err != nil {
			return nil, err
		}
	}
	if d == nil {
		a, err := p.compileSource(ctx, id, src)
		var ce *compileError
		if errors.As(err, &ce) {
			return nil, errStoredAuditor
		}
		return a, err
	}
	d.apply(&src)
	return p.compileSource(ctx, id, src)
}

// auditorSource is a stored (or draft) auditor before compiling.
type auditorSource struct {
	name, typ string
	channel   int64
	cfg       AuditorConfig
}

// errStoredAuditor: a saved auditor whose config no longer compiles.
var errStoredAuditor = errors.New("stored auditor config is invalid")

// compileError is an auditor that does not compile, reported on one field.
type compileError struct{ field, code, msg string }

func (e *compileError) Error() string { return e.field + ": " + e.msg }

func auditorSourceOf(db *gorm.DB, id int64) (auditorSource, error) {
	var src auditorSource
	var row model.Auditor
	err := db.Select("name, type, channel_id, config").Where("id = ? AND deleted_at IS NULL", id).Take(&row).Error
	src.name, src.typ, src.channel = row.Name, row.Type, row.ChannelID
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return src, errNotFound
	}
	if err != nil {
		return src, err
	}
	if json.Unmarshal(row.Config, &src.cfg) != nil {
		return src, errStoredAuditor
	}
	return src, nil
}

// compileSource compiles an auditor with its channel (which may be
// disabled: a try still runs).
func (p *Plugin) compileSource(ctx context.Context, id int64, src auditorSource) (*liveAuditor, error) {
	ch, err := p.loadChannel(ctx, src.channel)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, err
	}
	if want, ok := channelTypeOf[src.typ]; ok && ch != nil && ch.Type != want {
		return nil, &compileError{"channel_id", "type_mismatch", fmt.Sprintf("the auditor needs a %s channel / 该审核器需要 %s 类型的渠道", want, want)}
	}
	def := p.cfg.Load().jevDefault
	a, err := compileAuditor(id, src.name, src.typ, src.channel, src.cfg, ch, &def)
	if err != nil {
		return nil, &compileError{"config", "invalid", err.Error()}
	}
	return a, nil
}

func (p *Plugin) getJevDefaults(context.Context, *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	builtin, _ := DefaultQuestions(nil)
	c := p.cfg.Load()
	return pluginsdk.DataResponse(map[string]any{
		"builtin": builtin, "builtin_version": RulesVersionZH,
		"default": c.jevDefault, "default_version": RulesVersionOf(&c.jevDefault), "custom": c.JevDefaultSet != nil,
		"openai_categories": OpenAICategories, "openai_thresholds": DefaultThresholds,
		"llm_system_prompt": DefaultSystemPrompt, "llm_categories": DefaultCategories(),
	}), nil
}

// withParam returns a copy of req with a path parameter set.
func withParam(req *pluginv1.HTTPRequest, name string, id int64) *pluginv1.HTTPRequest {
	r := &pluginv1.HTTPRequest{Method: req.GetMethod(), Path: req.GetPath(), Caller: req.GetCaller(), PathParams: map[string]string{}}
	for k, v := range req.GetPathParams() {
		r.PathParams[k] = v
	}
	r.PathParams[name] = fmt.Sprint(id)
	return r
}

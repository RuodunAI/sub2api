package moderation

import (
	"bytes"
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

// FlowView is a flow as the API returns it.
type FlowView struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Enabled     bool            `json:"enabled"`
	Draft       json.RawMessage `json:"draft"`
	Published   json.RawMessage `json:"published"`
	Version     int             `json:"version"`
	PublishedAt *time.Time      `json:"published_at"`
	PublishedBy *int64          `json:"published_by"`
	Groups      []int64         `json:"groups"`
	// Scope is the audit scope (ScopeInput ...), null for a flow nobody
	// chose one for: it moderates the input, as ScopeNew (§72.4, 0.11.1).
	Scope *string `json:"scope"`
	// Accounts are the account bindings of the flow (§72.17).
	Accounts []AccountBinding `json:"accounts"`
	// Dirty: the draft differs from the published spec (a draft total
	// timeout of 0 means "computed at publish" and matches any value).
	Dirty bool `json:"dirty"`
	// PublishedAutoTimeout: the published version was drafted with
	// total_timeout_ms 0 ("automatic"); a discard puts 0 back in the draft.
	PublishedAutoTimeout bool `json:"published_auto_timeout"`
	// StageTimeoutSumMs is the automatic total timeout of the draft.
	StageTimeoutSumMs int        `json:"stage_timeout_sum_ms"`
	Stats24h          *FlowStats `json:"stats_24h,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// flowViewRow is a flow row with its bindings, as flowViews reads it.
type flowViewRow struct {
	ID                   int64
	Name                 string
	Enabled              bool
	Draft, Published     json.RawMessage
	Version              int
	PublishedAt          *time.Time
	PublishedBy          *int64
	Scope                *string
	GroupIDs             pgdb.Array[int64]
	Accounts             json.RawMessage
	PublishedAutoTimeout bool
	CreatedAt, UpdatedAt time.Time
}

// flowViews reads the live flows, or the one of id (0 = all).
func (p *Plugin) flowViews(db *gorm.DB, id int64) ([]FlowView, error) {
	q := db.Table(model.Prefix + "flows f").Select(`f.id, f.name, f.enabled, f.draft, f.published, f.version, f.published_at, f.published_by, f.scope,
			coalesce((SELECT array_agg(group_id ORDER BY group_id) FROM ` + model.Prefix + `flow_groups g WHERE g.flow_id = f.id), '{}') AS group_ids,
			coalesce((SELECT jsonb_agg(jsonb_build_object('group_id', a.group_id, 'account_id', a.account_id) ORDER BY a.account_id, a.group_id)
				FROM ` + model.Prefix + `flow_accounts a WHERE a.flow_id = f.id), '[]') AS accounts,
			coalesce((SELECT v.auto_timeout FROM ` + model.Prefix + `flow_versions v WHERE v.flow_id = f.id AND v.version = f.version), false) AS published_auto_timeout,
			f.created_at, f.updated_at`).Where("f.deleted_at IS NULL")
	if id != 0 {
		q = q.Where("f.id = ?", id)
	}
	var rows []flowViewRow
	err := q.Order("f.id").Scan(&rows).Error
	list := make([]FlowView, len(rows))
	for i, r := range rows {
		f := FlowView{ID: r.ID, Name: r.Name, Enabled: r.Enabled, Draft: r.Draft, Published: r.Published, Version: r.Version,
			PublishedAt: utcPtr(r.PublishedAt), PublishedBy: r.PublishedBy, Scope: r.Scope, Groups: r.GroupIDs,
			PublishedAutoTimeout: r.PublishedAutoTimeout, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
		if err := json.Unmarshal(r.Accounts, &f.Accounts); err != nil {
			return nil, err
		}
		f.Dirty = draftChanged(f.Draft, f.Published)
		list[i] = f
	}
	if err != nil || len(list) == 0 {
		return list, err
	}
	timeouts, err := auditorTimeouts(db)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if spec, err := ParseSpec(list[i].Draft); err == nil {
			list[i].StageTimeoutSumMs = spec.DefaultTotalTimeoutMs(func(a int64) int { return timeouts[a] })
		}
	}
	return list, nil
}

// draftChanged compares a draft with the published spec; a draft total
// timeout of 0 takes the published value (new-api moderationDraftChanged).
func draftChanged(draft, published []byte) bool {
	if len(published) == 0 {
		return true
	}
	d, err1 := ParseSpec(draft)
	pub, err2 := ParseSpec(published)
	if err1 != nil || err2 != nil {
		return !bytes.Equal(draft, published)
	}
	if d.TotalTimeoutMs == 0 {
		d.TotalTimeoutMs = pub.TotalTimeoutMs
	}
	a, _ := json.Marshal(d)
	b, _ := json.Marshal(pub)
	return !bytes.Equal(a, b)
}

func (p *Plugin) listFlows(ctx context.Context, _ *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := p.flowViews(db, 0)
	if err != nil {
		return nil, err
	}
	stats, err := p.flowStats24h(ctx, db)
	if err != nil {
		return nil, err
	}
	for i := range list {
		s := stats[list[i].ID]
		list[i].Stats24h = &s
	}
	return pluginsdk.DataResponse(list), nil
}

func (p *Plugin) getFlow(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := p.flowViews(db, id)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return notFound("flow"), nil
	}
	return pluginsdk.DataResponse(list[0]), nil
}

// flowInput is the body of a create or an update. Scope, when given, takes
// effect at once, without a publish, like enabled and the bindings: it is a
// column of the flow, not a part of its spec (scope.go). A create without it
// gives ScopeNew, an update without it leaves the scope as it is.
type flowInput struct {
	Name  string          `json:"name"`
	Draft json.RawMessage `json:"draft"`
	Scope *string         `json:"scope"`
}

// check validates the name and the scope.
func (in *flowInput) check() (string, pluginsdk.FieldErrors) {
	name, errs := flowName(in.Name)
	if in.Scope != nil && !validScope(*in.Scope) {
		errs = errs.Add("scope", "invalid_scope", "scope must be input, tools or all / 审核范围只能是 input、tools 或 all")
	}
	return name, errs
}

// flowExists reports whether the flow id exists and is not deleted.
func flowExists(tx *gorm.DB, id int64) (bool, error) {
	return pgdb.Exists(tx, tx.Model(&model.Flow{}).Where("id = ? AND deleted_at IS NULL", id))
}

// checkDraft parses and validates a draft against the live auditors.
func checkDraft(tx *gorm.DB, raw json.RawMessage) (*Spec, error) {
	spec, err := ParseSpec(raw)
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := tx.Model(&model.Auditor{}).Where("deleted_at IS NULL").Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	exists := map[int64]bool{}
	for _, id := range ids {
		exists[id] = true
	}
	if spec.ModelFilter.Models == nil {
		spec.ModelFilter.Models = []string{}
	}
	return spec, spec.Validate(func(id int64) bool { return exists[id] })
}

func flowName(s string) (string, pluginsdk.FieldErrors) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 64 {
		return s, pluginsdk.FieldErrors{}.Add("name", "invalid", "name is required, at most 64 characters / 名称必填，最多 64 字")
	}
	return s, nil
}

func (p *Plugin) createFlow(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	in := flowInput{Scope: new(ScopeNew)}
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	name, errs := in.check()
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid flow / 流程参数无效", errs), nil
	}
	var id int64
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		spec, err := checkDraft(tx, in.Draft)
		if err != nil {
			resp = pluginsdk.FieldErrorResponse("invalid flow / 流程参数无效", pluginsdk.FieldErrors{}.Add("draft", "invalid", err.Error()))
			return errRollback
		}
		raw, _ := json.Marshal(spec)
		now := p.now().UTC()
		id, err = insertRow(tx, &model.Flow{}, map[string]any{"name": name, "draft": json.RawMessage(raw), "scope": in.Scope,
			"created_at": now, "updated_at": now})
		return err
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getFlow(ctx, withParam(req, "id", id))
}

// updateFlow saves the name and the draft, the published version stays
// until publish; and the scope, which applies at once (flowInput).
func (p *Plugin) updateFlow(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var in flowInput
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	name, errs := in.check()
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid flow / 流程参数无效", errs), nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		spec, err := checkDraft(tx, in.Draft)
		if err != nil {
			resp = pluginsdk.FieldErrorResponse("invalid flow / 流程参数无效", pluginsdk.FieldErrors{}.Add("draft", "invalid", err.Error()))
			return errRollback
		}
		raw, _ := json.Marshal(spec)
		set := map[string]any{"name": name, "draft": json.RawMessage(raw), "updated_at": p.now().UTC()}
		if in.Scope != nil {
			set["scope"] = *in.Scope
		}
		res := tx.Model(&model.Flow{}).Where("id = ? AND deleted_at IS NULL", id).Updates(set)
		if res.Error == nil && res.RowsAffected == 0 {
			resp = notFound("flow")
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
	return p.getFlow(ctx, req)
}

func (p *Plugin) deleteFlow(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		var inUse bool
		err := tx.Table(model.Prefix+"flows f").Select(`f.enabled AND (EXISTS (SELECT 1 FROM `+model.Prefix+`flow_groups g WHERE g.flow_id = f.id)
				OR EXISTS (SELECT 1 FROM `+model.Prefix+`flow_accounts a WHERE a.flow_id = f.id))`).
			Where("f.id = ? AND f.deleted_at IS NULL", id).Clauses(forUpdate()).Take(&inUse).Error
		if err == nil && inUse {
			resp = pluginsdk.ErrorResponse(http.StatusConflict, "flow_in_use",
				"an enabled flow bound to groups or accounts cannot be deleted; disable it or unbind it first / 启用且已绑定分组或账号的流程不能删除，请先停用或解绑")
			return errRollback
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		res := tx.Model(&model.Flow{}).Where("id = ? AND deleted_at IS NULL", id).Update("deleted_at", p.now().UTC())
		if res.Error == nil && res.RowsAffected == 0 {
			resp = notFound("flow")
			return errRollback
		}
		if res.Error != nil {
			return res.Error
		}
		if err := tx.Where("flow_id = ?", id).Delete(&model.FlowGroup{}).Error; err != nil {
			return err
		}
		return tx.Where("flow_id = ?", id).Delete(&model.FlowAccount{}).Error
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(map[string]any{"id": id, "deleted": true}), nil
}

// setFlowGroups replaces the groups of a flow: {groups:[ids], force?}. A
// group bound to another flow is a 409 group_conflict unless force moves it.
func (p *Plugin) setFlowGroups(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var in struct {
		Groups []int64 `json:"groups"`
		Force  bool    `json:"force"`
	}
	if err := json.Unmarshal(req.GetBody(), &in); err != nil {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", "invalid JSON body / 请求体不是合法 JSON"), nil
	}
	for _, g := range in.Groups {
		if g < 0 {
			return pluginsdk.FieldErrorResponse("invalid groups / 分组无效", pluginsdk.FieldErrors{}.Add("groups", "invalid", "group ids must be >= 0 / 分组 id 必须 >= 0")), nil
		}
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		exists, err := flowExists(tx, id)
		if err != nil {
			return err
		}
		if !exists {
			resp = notFound("flow")
			return errRollback
		}
		type conflict struct {
			GroupID int64 `json:"group_id"`
			FlowID  int64 `json:"flow_id"`
		}
		groups := pgdb.Array[int64](in.Groups)
		conflicts := []conflict{}
		err = tx.Model(&model.FlowGroup{}).Where("group_id = ANY(?) AND flow_id <> ?", groups, id).Order("group_id").Scan(&conflicts).Error
		if err != nil {
			return err
		}
		if len(conflicts) > 0 && !in.Force {
			resp = errorWithDetails(http.StatusConflict, "group_conflict", "groups are bound to other flows / 分组已绑定其他流程", map[string]any{"conflicts": conflicts})
			return errRollback
		}
		if err := tx.Where("flow_id = ? OR group_id = ANY(?)", id, groups).Delete(&model.FlowGroup{}).Error; err != nil {
			return err
		}
		for _, g := range in.Groups {
			err := tx.Clauses(clause.OnConflict{Columns: columns("group_id"), DoUpdates: clause.AssignmentColumns([]string{"flow_id"})}).
				Create(&model.FlowGroup{GroupID: g, FlowID: id}).Error
			if err != nil {
				return err
			}
		}
		return nil
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getFlow(ctx, req)
}

// publishFlow publishes the draft: {expected_version}.
func (p *Plugin) publishFlow(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var in struct {
		ExpectedVersion *int `json:"expected_version"`
	}
	if err := json.Unmarshal(req.GetBody(), &in); err != nil || in.ExpectedVersion == nil {
		return pluginsdk.FieldErrorResponse("invalid publish / 发布参数无效",
			pluginsdk.FieldErrors{}.Add("expected_version", "required", "expected_version is required / 请带上当前版本号")), nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		var f model.Flow
		err := tx.Clauses(forUpdate()).Select("version, draft").Where("id = ? AND deleted_at IS NULL", id).Take(&f).Error
		version, draft := f.Version, f.Draft
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp = notFound("flow")
			return errRollback
		}
		if err != nil {
			return err
		}
		if version != *in.ExpectedVersion {
			resp = pluginsdk.ErrorResponse(http.StatusConflict, "version_conflict",
				fmt.Sprintf("the flow is at version %d / 流程已被他人发布到版本 %d，请刷新", version, version))
			return errRollback
		}
		spec, err := checkDraft(tx, draft)
		if err != nil {
			resp = pluginsdk.FieldErrorResponse("invalid flow / 流程参数无效", pluginsdk.FieldErrors{}.Add("draft", "invalid", err.Error()))
			return errRollback
		}
		return publishSpec(tx, id, version+1, spec, p.now().UTC(), callerID(req))
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.getFlow(ctx, req)
}

// publishSpec publishes spec as the given version of flow id and makes it
// the draft. A total_timeout_ms of 0 ("automatic") stays 0 in the draft and
// is resolved from the channel timeouts in the published spec only, so the
// draft keeps following its auditors; the version row remembers it
// (auto_timeout) for a rollback or a discard.
func publishSpec(tx *gorm.DB, id int64, version int, spec *Spec, now time.Time, by *int64) error {
	draft, _ := json.Marshal(spec)
	auto := spec.TotalTimeoutMs == 0
	if err := fillAutoTimeout(tx, spec); err != nil {
		return err
	}
	raw, _ := json.Marshal(spec)
	err := tx.Model(&model.Flow{}).Where("id = ?", id).Updates(map[string]any{"draft": json.RawMessage(draft), "published": json.RawMessage(raw),
		"version": version, "published_at": now, "published_by": by, "updated_at": now}).Error
	if err != nil {
		return err
	}
	return tx.Create(&model.FlowVersion{FlowID: id, Version: version, Spec: raw, PublishedAt: now, PublishedBy: by, AutoTimeout: auto}).Error
}

// fillAutoTimeout gives a spec without a total timeout the automatic one
// (the sum of each stage's slowest channel).
func fillAutoTimeout(db *gorm.DB, spec *Spec) error {
	if spec.TotalTimeoutMs > 0 {
		return nil
	}
	timeouts, err := auditorTimeouts(db)
	if err != nil {
		return err
	}
	spec.TotalTimeoutMs = spec.DefaultTotalTimeoutMs(func(a int64) int { return timeouts[a] })
	return nil
}

// auditorTimeouts maps live auditors to their channel timeout.
func auditorTimeouts(db *gorm.DB) (map[int64]int, error) {
	var rows []struct {
		ID        int64
		Type      string
		TimeoutMs int
	}
	err := db.Table(model.Prefix + "auditors a").Select("a.id, c.type, c.timeout_ms").Joins("JOIN " + model.Prefix + "channels c ON c.id = a.channel_id").
		Where("a.deleted_at IS NULL").Scan(&rows).Error
	out := make(map[int64]int, len(rows))
	for _, r := range rows {
		out[r.ID] = cmp.Or(max(r.TimeoutMs, 0), defaultChannelTimeoutMs[r.Type])
	}
	return out, err
}

// setFlowEnabled and setFlowScope change a column that takes effect at once,
// without a publish (PUT /flows/:id/enabled, /flows/:id/scope).
func (p *Plugin) setFlowEnabled(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(req.GetBody(), &in); err != nil || in.Enabled == nil {
		return pluginsdk.FieldErrorResponse("invalid body / 参数无效", pluginsdk.FieldErrors{}.Add("enabled", "required", "enabled is required / 请指定 enabled")), nil
	}
	return p.setFlowColumn(ctx, req, "enabled", *in.Enabled)
}

func (p *Plugin) setFlowScope(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(req.GetBody(), &in); err != nil || !validScope(in.Scope) {
		return pluginsdk.FieldErrorResponse("invalid body / 参数无效", pluginsdk.FieldErrors{}.Add("scope", "invalid_scope", "scope must be input, tools or all / 审核范围只能是 input、tools 或 all")), nil
	}
	return p.setFlowColumn(ctx, req, "scope", in.Scope)
}

// setFlowColumn sets column (a constant name) of the flow of the path to v.
func (p *Plugin) setFlowColumn(ctx context.Context, req *pluginv1.HTTPRequest, column string, v any) (*pluginv1.HTTPResponse, error) {
	id, bad := parseID(req, "id")
	if bad != nil {
		return bad, nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		res := tx.Model(&model.Flow{}).Where("id = ? AND deleted_at IS NULL", id).Updates(map[string]any{column: v, "updated_at": p.now().UTC()})
		if res.Error == nil && res.RowsAffected == 0 {
			resp = notFound("flow")
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
	return p.getFlow(ctx, req)
}

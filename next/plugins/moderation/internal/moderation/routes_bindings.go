package moderation

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Sub2API-Devs/sup2api/next/plugins/moderation/internal/model"
	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
)

// Binding kinds (CONTRACTS §72.17), most specific first.
const (
	BindingGroupAccount = "group_account" // an account within a group
	BindingAccount      = "account"       // an account in any group
	BindingGroup        = "group"         // every account of a group; group 0 = the default
)

// Binding is one binding of the whole table: account_id 0 = a group binding
// (flow_groups), group_id 0 with an account = the account in any group.
// flow_id 0 = not moderated. Inactive: the flow is disabled, unpublished,
// gone or published with a spec this node cannot read, so the binding does
// not apply and a wider one decides. It is what the node does: the flow is
// not among the flows it runs (snapshot.flows). FallbackReason says which
// (FallbackDisabled ..., "" when the binding applies) and EffectiveFlowID is
// the flow a request the binding covers runs, 0 = none (0.10.0): its own
// flow, or the flow of the wider binding that decides - for an account in
// any group, that of a group without a binding of its own.
type Binding struct {
	Kind            string `json:"kind"`
	GroupID         int64  `json:"group_id"`
	AccountID       int64  `json:"account_id"`
	FlowID          int64  `json:"flow_id"`
	Inactive        bool   `json:"inactive,omitempty"`
	EffectiveFlowID int64  `json:"effective_flow_id"`
	FallbackReason  string `json:"fallback_reason"`
}

func bindingKind(group, account int64) string {
	switch {
	case account == 0:
		return BindingGroup
	case group == 0:
		return BindingAccount
	default:
		return BindingGroupAccount
	}
}

// BindingsView is GET /bindings. AccountBindingsActive: the gateway.attempt
// hook is granted; without it the account bindings are kept but every
// request goes by its group.
type BindingsView struct {
	Bindings              []Binding `json:"bindings"`
	AccountBindingsActive bool      `json:"account_bindings_active"`
}

// AccountBinding is an account binding of one flow (FlowView.accounts).
type AccountBinding struct {
	GroupID   int64 `json:"group_id"`
	AccountID int64 `json:"account_id"`
}

func (p *Plugin) listBindings(ctx context.Context, _ *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	db, err := p.db(ctx)
	if err != nil {
		return pluginsdk.Unavailable(err.Error()), nil
	}
	list, err := loadBindings(db, p.snap.Load())
	if err != nil {
		return nil, err
	}
	return pluginsdk.DataResponse(BindingsView{Bindings: list, AccountBindingsActive: p.attemptHook.Load()}), nil
}

// loadBindings reads both tables, account bindings first (by account, then
// group), then the group bindings. A binding is inactive when its flow is
// not one snap runs: the same test that makes the request fall back to a
// wider binding (bindGroup, bindAccount: staleReason), so a published and
// enabled flow whose spec was dropped from the snapshot is not reported as
// applying.
func loadBindings(db *gorm.DB, snap *snapshot) ([]Binding, error) {
	var rows []model.FlowAccount
	both := db.Raw("(?) UNION ALL (?)", db.Model(&model.FlowAccount{}).Select("group_id, account_id, flow_id"),
		db.Model(&model.FlowGroup{}).Select("group_id, 0 AS account_id, flow_id"))
	err := db.Table("(?) AS b", both).Order("b.account_id = 0, b.account_id, b.group_id").Scan(&rows).Error
	list := make([]Binding, len(rows))
	for i, r := range rows {
		b := Binding{GroupID: r.GroupID, AccountID: r.AccountID, FlowID: r.FlowID}
		b.Kind = bindingKind(b.GroupID, b.AccountID)
		b.FallbackReason = snap.staleReason(b.FlowID)
		b.Inactive = b.FallbackReason != ""
		b.EffectiveFlowID = snap.effectiveFlow(b.GroupID, b.AccountID)
		list[i] = b
	}
	return list, err
}

// putBinding binds {group_id, account_id} to {flow_id} (0 = not moderated).
// A binding that exists with another flow is a 409 binding_exists unless
// replace is set. Saving takes effect at once and creates no version.
func (p *Plugin) putBinding(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	var in struct {
		GroupID   int64 `json:"group_id"`
		AccountID int64 `json:"account_id"`
		FlowID    int64 `json:"flow_id"`
		Replace   bool  `json:"replace"`
	}
	if bad := decodeBody(req, &in); bad != nil {
		return bad, nil
	}
	errs := pluginsdk.FieldErrors{}
	for _, f := range []struct {
		name string
		v    int64
	}{{"group_id", in.GroupID}, {"account_id", in.AccountID}, {"flow_id", in.FlowID}} {
		if f.v < 0 {
			errs = errs.Add(f.name, "invalid", f.name+" must be >= 0 / "+f.name+" 必须 >= 0")
		}
	}
	if len(errs) > 0 {
		return pluginsdk.FieldErrorResponse("invalid binding / 绑定无效", errs), nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		if in.FlowID > 0 {
			exists, err := flowExists(tx, in.FlowID)
			if err != nil {
				return err
			}
			if !exists {
				resp = notFound("flow")
				return errRollback
			}
		}
		current, found, err := bindingFlow(tx, in.GroupID, in.AccountID)
		if err != nil {
			return err
		}
		if found && current != in.FlowID && !in.Replace {
			resp = errorWithDetails(http.StatusConflict, "binding_exists", "the binding exists with another flow / 该绑定已指向其他流程",
				map[string]any{"flow_id": current})
			return errRollback
		}
		if in.AccountID == 0 {
			return tx.Clauses(clause.OnConflict{Columns: columns("group_id"), DoUpdates: clause.AssignmentColumns([]string{"flow_id"})}).
				Create(&model.FlowGroup{GroupID: in.GroupID, FlowID: in.FlowID}).Error
		}
		return tx.Clauses(clause.OnConflict{Columns: columns("group_id", "account_id"), DoUpdates: clause.AssignmentColumns([]string{"flow_id"})}).
			Create(&model.FlowAccount{GroupID: in.GroupID, AccountID: in.AccountID, FlowID: in.FlowID}).Error
	})
	if resp != nil {
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return p.listBindings(ctx, req)
}

// bindingFlow reads the flow of one binding.
func bindingFlow(tx *gorm.DB, group, account int64) (int64, bool, error) {
	var flow int64
	var err error
	if account == 0 {
		err = tx.Model(&model.FlowGroup{}).Select("flow_id").Where("group_id = ?", group).Take(&flow).Error
	} else {
		err = tx.Model(&model.FlowAccount{}).Select("flow_id").Where("group_id = ? AND account_id = ?", group, account).Take(&flow).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	return flow, err == nil, err
}

func (p *Plugin) deleteBinding(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	group, err1 := strconv.ParseInt(pluginsdk.Param(req, "group_id"), 10, 64)
	account, err2 := strconv.ParseInt(pluginsdk.Param(req, "account_id"), 10, 64)
	if err1 != nil || err2 != nil || group < 0 || account < 0 {
		return pluginsdk.ErrorResponse(http.StatusBadRequest, "invalid_argument", "group_id and account_id must be integers >= 0 / group_id 和 account_id 必须是 >= 0 的整数"), nil
	}
	var resp *pluginv1.HTTPResponse
	err := p.configTx(ctx, func(tx *gorm.DB) error {
		var res *gorm.DB
		if account == 0 {
			res = tx.Where("group_id = ?", group).Delete(&model.FlowGroup{})
		} else {
			res = tx.Where("group_id = ? AND account_id = ?", group, account).Delete(&model.FlowAccount{})
		}
		if res.Error == nil && res.RowsAffected == 0 {
			resp = pluginsdk.ErrorResponse(http.StatusNotFound, "not_found", "binding not found / 绑定不存在")
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
	return p.listBindings(ctx, req)
}

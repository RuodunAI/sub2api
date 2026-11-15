package pluginsdk

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
)

// Plugin resource scope (HTTPRequest.action_scope; CONTRACTS §21.0). A
// userPermissions entry that declares a scope is granted for every row, for
// the rows the holder created (own), and/or for listed values
// ("<resource>/<value>"). On a route whose permission declares one, the core
// admits a holder of any scope, checks the values the route's scopeFrom
// names, and attaches the caller's scope to the request. The plugin filters
// lists by it, and MUST check own against its own created_by: the core does
// not know who created a row.
//
//	scope := pluginsdk.RequestScope(req)
//	// a list
//	rows, err := db.Query(ctx, `SELECT … FROM flows WHERE $1 OR id::text = ANY($2) OR ($3 AND created_by = $4)`,
//		scope.All, scope.Values, scope.Own, req.GetCaller().GetUserId())
//	// one row
//	if resp := pluginsdk.RequireScope(req, flow.ID, flow.CreatedBy); resp != nil {
//		return resp, nil
//	}

// Scope is a console user's scope of one scoped plugin permission. The zero
// value reaches nothing.
type Scope struct {
	// Permission is the qualified action, without a scope:
	// "plugin.<key>:flow:manage".
	Permission string
	// All: every row.
	All bool
	// Own: the rows the user created, which only the plugin can tell.
	Own bool
	// Values are the listed values, sorted, once each.
	Values []string
	userID int64 // the caller, for Own
}

// RequestScope is the scope the core attached to req for the route's
// permission. A route whose permission declares no scope (or a core that
// sent none) gets the zero Scope: fail closed.
func RequestScope(req *pluginv1.HTTPRequest) Scope {
	s := req.GetActionScope()
	return Scope{Permission: s.GetPermission(), All: s.GetAll(), Own: s.GetOwn(), Values: s.GetValues(),
		userID: req.GetCaller().GetUserId()}
}

// Allows reports whether the user may act on the row value, created by
// createdBy (0 when the row records no creator, which only All and Values
// reach).
func (s Scope) Allows(value string, createdBy int64) bool {
	return s.All || slices.Contains(s.Values, value) || s.Own && createdBy > 0 && createdBy == s.userID
}

// RequireScope is nil when req's caller may act on the row value, created
// by createdBy; otherwise the answer to send: 401 without a user, 404 out of
// scope (as if the row did not exist, as the core answers on its routes).
func RequireScope(req *pluginv1.HTTPRequest, value string, createdBy int64) *pluginv1.HTTPResponse {
	if req.GetCaller().GetUserId() <= 0 {
		return Unauthenticated()
	}
	if !RequestScope(req).Allows(value, createdBy) {
		return ErrorResponse(http.StatusNotFound, "not_found", "not found / 不存在")
	}
	return nil
}

// The limit of a scope options query, as the core's GET /lookup.
const (
	scopeOptionsDefaultLimit = 20
	scopeOptionsMaxLimit     = 50
)

// ScopeOptionsQuery is one call of a scope options route (manifest
// userPermissions[].scope.options): from the core, listing the candidates
// of the permission a role editor configures.
type ScopeOptionsQuery struct {
	Text string   // q, trimmed
	IDs  []string // ids=; when given, Text does not apply and Limit is len(IDs)
	// Limit is 1..50.
	Limit int
	// Scope is the caller's scope of the permission being configured (not of
	// the options route's own permission): list only rows in it.
	Scope Scope
}

// ScopeOption is one candidate: the id is the <value> of a
// "<resource>/<value>" grant.
type ScopeOption struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Hint   string `json:"hint,omitempty"`
	Status string `json:"status,omitempty"`
}

// ScopeOptions wraps fn as a scope options route: it parses the query and
// answers DataResponse({items, more}) with fn's rows, cut to Limit (more is
// set when fn reported more or rows were cut; never for IDs). fn filters by
// q.Scope (All; Values; Own = rows the caller created) and never sees a
// request without a user (401).
func ScopeOptions(fn func(ctx context.Context, q ScopeOptionsQuery) ([]ScopeOption, bool, error)) HandlerFunc {
	return func(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
		if req.GetCaller().GetUserId() <= 0 {
			return Unauthenticated(), nil
		}
		q := ScopeOptionsQuery{Text: strings.TrimSpace(Query(req, "q")), Limit: scopeOptionsDefaultLimit, Scope: RequestScope(req)}
		if n, err := strconv.Atoi(Query(req, "limit")); err == nil && n >= 1 && n <= scopeOptionsMaxLimit {
			q.Limit = n
		}
		for _, id := range strings.Split(Query(req, "ids"), ",") {
			if id = strings.TrimSpace(id); id != "" {
				q.IDs = append(q.IDs, id)
			}
		}
		if q.IDs != nil {
			q.Text, q.Limit = "", len(q.IDs)
		}
		items, more, err := fn(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(items) > q.Limit {
			items, more = items[:q.Limit], true
		}
		if items == nil {
			items = []ScopeOption{}
		}
		return DataResponse(map[string]any{"items": items, "more": more && q.IDs == nil}), nil
	}
}

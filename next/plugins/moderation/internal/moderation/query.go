package moderation

import (
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sqlFilter accumulates the conditions of a WHERE clause from query
// parameters; errs collects the invalid parameters. A condition is
// constant SQL text with ? parameters, or a clause on a column the code
// names: never text built from the request.
type sqlFilter struct {
	conds []clause.Expression
	errs  pluginsdk.FieldErrors
}

// add adds the condition cond, its ? placeholders taking args.
func (f *sqlFilter) add(cond pgdb.SQL, args ...any) {
	f.conds = append(f.conds, clause.Expr{SQL: string(cond), Vars: args})
}

// eq adds "column = v".
func (f *sqlFilter) eq(column string, v any) {
	f.conds = append(f.conds, clause.Eq{Column: clause.Column{Name: column}, Value: v})
}

// scope applies the conditions to db, for Scopes.
func (f *sqlFilter) scope(db *gorm.DB) *gorm.DB {
	for _, c := range f.conds {
		db = db.Where(c)
	}
	return db
}

// id adds "column = ?" for a non-negative integer parameter.
func (f *sqlFilter) id(req *pluginv1.HTTPRequest, name, column string) {
	if v, ok := f.queryID(req, name); ok {
		f.eq(column, v)
	}
}

// queryID is queryID collecting the error in f.errs.
func (f *sqlFilter) queryID(req *pluginv1.HTTPRequest, name string) (int64, bool) {
	id, ok, bad := queryID(req, name)
	if bad != nil {
		f.errs = append(f.errs, bad)
	}
	return id, ok
}

// queryID reads a non-negative integer parameter; ok = present and valid,
// bad = present and invalid.
func queryID(req *pluginv1.HTTPRequest, name string) (id int64, ok bool, bad *pluginv1.FieldError) {
	v := pluginsdk.Query(req, name)
	if v == "" {
		return 0, false, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return 0, false, &pluginv1.FieldError{Field: name, Code: "invalid", Message: name + " must be a non-negative integer / " + name + " 必须是非负整数"}
	}
	return id, true, nil
}

// queryTime reads an RFC 3339 parameter; zero when absent or invalid.
func (f *sqlFilter) queryTime(req *pluginv1.HTTPRequest, name string) time.Time {
	v := pluginsdk.Query(req, name)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		f.errs = f.errs.Add(name, "format", name+" must be RFC 3339 / "+name+" 必须是 RFC 3339 时间")
		return time.Time{}
	}
	return t.UTC()
}

// times adds "created_at >= from" and "created_at < to" for the parameters
// from and to.
func (f *sqlFilter) times(req *pluginv1.HTTPRequest) (from, to time.Time) {
	from, to = f.queryTime(req, "from"), f.queryTime(req, "to")
	if !from.IsZero() {
		f.add("created_at >= ?", from)
	}
	if !to.IsZero() {
		f.add("created_at < ?", to)
	}
	return from, to
}

// flag adds "column = true" for the parameter name = true and
// "column = false" for false (column is NOT NULL).
func (f *sqlFilter) flag(req *pluginv1.HTTPRequest, name, column string) {
	switch v := pluginsdk.Query(req, name); v {
	case "":
	case "true", "false":
		f.eq(column, v == "true")
	default:
		f.errs = f.errs.Add(name, "enum", name+" must be true or false / "+name+" 取值为 true、false")
	}
}

// maxIDList caps the ids of a list parameter.
const maxIDList = 500

// idList adds "column = ANY(?)" for a comma list of user ids (the
// console's username search resolved through the core). "0" matches
// nobody: the search found no user.
func (f *sqlFilter) idList(req *pluginv1.HTTPRequest, name, column string) {
	v := pluginsdk.Query(req, name)
	if v == "" {
		return
	}
	ids, ok := parseIDList(v)
	if !ok {
		f.errs = f.errs.Add(name, "invalid", name+" is a comma list of up to 500 user ids (0 = none) / "+name+" 为逗号分隔的用户 id，最多 500 个（0 表示无匹配）")
		return
	}
	f.conds = append(f.conds, clause.Expr{SQL: "? = ANY(?)", Vars: []any{clause.Column{Name: column}, pgdb.Array[int64](ids)}})
}

// parseIDList reads a comma list of at most maxIDList ids (>= 0).
func parseIDList(v string) ([]int64, bool) {
	parts := strings.Split(v, ",")
	if len(parts) > maxIDList {
		return nil, false
	}
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id < 0 {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

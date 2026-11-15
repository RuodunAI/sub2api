package audit

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// RegisterRoutes mounts GET /audit-logs: newest first, filtered by action,
// target_type, target_id, user_id and the RFC 3339 created_at range from/to.
// Each row carries user_email and user_name of its actor (null for the
// system).
func RegisterRoutes(r *httpapi.Router, db *store.DB) {
	r.Perm("GET", "/audit-logs", "audit:read", func(c *gin.Context) {
		var f logFilter
		for _, x := range []struct{ q, cond string }{{"action", "a.action = ?"}, {"target_type", "a.target_type = ?"}, {"target_id", "a.target_id = ?"}} {
			if v := c.Query(x.q); v != "" {
				f = append(f, logCond{x.cond, v})
			}
		}
		if v := c.Query("user_id"); v != "" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("invalid user_id"))
				return
			}
			f = append(f, logCond{"a.user_id = ?", id})
		}
		for _, x := range []struct{ q, cond string }{{"from", "a.created_at >= ?"}, {"to", "a.created_at < ?"}} {
			v := c.Query(x.q)
			if v == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("invalid "+x.q+" (RFC 3339)"))
				return
			}
			f = append(f, logCond{x.cond, t})
		}
		page, size := httpapi.Pagination(c)
		g := store.G(c.Request.Context(), db.Pool)
		var total int64
		if err := f.logs(g).Count(&total).Error; err != nil {
			httpapi.Fail(c, err)
			return
		}
		items := []json.RawMessage{}
		if err := logPage(g, f, size, (page-1)*size, &items).Error; err != nil {
			httpapi.Fail(c, err)
			return
		}
		httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
	})
}

// logCond is one condition on audit_logs a with its argument.
type logCond struct {
	cond string
	arg  any
}

// logFilter is the conditions of a listing, ANDed.
type logFilter []logCond

// logs is the rows of audit_logs a that f keeps.
func (f logFilter) logs(db *gorm.DB) *gorm.DB {
	db = db.Table("audit_logs a")
	for _, c := range f {
		db = db.Where(c.cond, c.arg)
	}
	return db
}

// logPage reads into dst one page of the rows f keeps, newest first, each
// row as JSON with the e-mail and name of its actor.
func logPage(db *gorm.DB, f logFilter, limit, offset int, dst *[]json.RawMessage) *gorm.DB {
	return f.logs(db).Joins("LEFT JOIN users u ON u.id = a.user_id").Order("a.id DESC").
		Limit(limit).Offset(offset).Pluck("to_jsonb(a) || jsonb_build_object('user_email', u.email, 'user_name', u.display_name)", dst)
}

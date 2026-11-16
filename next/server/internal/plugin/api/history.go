package api

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
)

// listRollouts serves both the per-plugin and cross-plugin history. Completed
// node results are snapshots; current rollouts use the live controller view.
func (a *API) listRollouts(c *gin.Context) {
	key := c.Param("key")
	var since *time.Time
	if raw := c.Query("since"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("invalid since"))
			return
		}
		since = &value
	}
	page, size := httpapi.Pagination(c)
	g := store.G(ctx(c), a.d.DB.Gorm)
	var total int64
	if err := rollouts(g, key, since).Count(&total).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	items, err := store.CollectRows(rolloutPage(g, key, since, size, (page-1)*size), func(rows *sql.Rows) (map[string]any, error) {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var item map[string]any
		return item, json.Unmarshal(raw, &item)
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	if a.d.Rollout != nil {
		for _, item := range items {
			if item["phase"] != "preparing" && item["phase"] != "activating" {
				continue
			}
			current, readErr := a.d.Rollout.Current(ctx(c), item["plugin_key"].(string))
			if readErr != nil {
				httpapi.Fail(c, readErr)
				return
			}
			if current != nil && float64(current.ID) == item["id"] {
				item["nodes"] = current.Nodes
			}
		}
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
}

// rollouts selects the rollouts (as r) of a plugin key ("": all) created at
// or after since (nil: any time).
func rollouts(db *gorm.DB, key string, since *time.Time) *gorm.DB {
	q := db.Table("plugin_rollouts AS r")
	if key != "" {
		q = q.Where("r.plugin_key = ?", key)
	}
	if since != nil {
		q = q.Where("r.created_at >= ?", *since)
	}
	return q
}

// rolloutPage selects a page of the rollouts, newest first, each one a JSON
// document with its coordinator and node results.
func rolloutPage(db *gorm.DB, key string, since *time.Time, limit, offset int) *gorm.DB {
	nodes := db.Table("plugin_rollout_nodes AS n").Select("jsonb_agg(to_jsonb(n) ORDER BY n.node_id, n.boot_id)").Where("n.rollout_id = r.id")
	return rollouts(db, key, since).
		Select("to_jsonb(r) || jsonb_build_object('coordinator', r.coordinator_node_id, 'nodes', COALESCE((?), '[]'::jsonb))", nodes).
		Order("r.id DESC").Limit(limit).Offset(offset)
}

// history selects the history (as h) of a plugin key, of rollout id (0:
// any).
func history(db *gorm.DB, key string, rolloutID int64) *gorm.DB {
	q := db.Table("plugin_history AS h").Where("h.plugin_key = ?", key)
	if rolloutID != 0 {
		q = q.Where("h.rollout_id = ?", rolloutID)
	}
	return q
}

func (a *API) pluginHistory(c *gin.Context) {
	var rolloutID int64
	if raw := c.Query("rollout_id"); raw != "" {
		var err error
		rolloutID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || rolloutID <= 0 {
			httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("invalid rollout_id"))
			return
		}
	}
	page, size := httpapi.Pagination(c)
	key := c.Param("key")
	g := store.G(ctx(c), a.d.DB.Gorm)
	var total int64
	if err := history(g, key, rolloutID).Count(&total).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	docs := history(g, key, rolloutID).Select("to_jsonb(h)").Order("h.id DESC").Limit(size).Offset((page - 1) * size)
	items, err := store.CollectRows(docs, func(rows *sql.Rows) (json.RawMessage, error) {
		var item json.RawMessage
		err := rows.Scan(&item)
		return item, err
	})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
}

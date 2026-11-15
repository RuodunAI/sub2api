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
	if err := g.Raw(`SELECT count(*) FROM plugin_rollouts`+rolloutsWhere, key, key, since, since).Row().Scan(&total); err != nil {
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

// rolloutsWhere keeps the rollouts of a plugin key ("": all) created at or
// after since (nil: any time); it takes key, key, since, since.
const rolloutsWhere = ` WHERE (?='' OR plugin_key=?) AND (?::timestamptz IS NULL OR created_at >= ?)`

// rolloutPage selects a page of rolloutsWhere's rollouts, newest first, each
// one a JSON document with its coordinator and node results.
func rolloutPage(db *gorm.DB, key string, since *time.Time, limit, offset int) *gorm.DB {
	return db.Raw(`SELECT to_jsonb(r) || jsonb_build_object('coordinator',r.coordinator_node_id,'nodes',
        COALESCE((SELECT jsonb_agg(to_jsonb(n) ORDER BY n.node_id,n.boot_id) FROM plugin_rollout_nodes n WHERE n.rollout_id=r.id),'[]'::jsonb))
        FROM plugin_rollouts r`+rolloutsWhere+` ORDER BY id DESC LIMIT ? OFFSET ?`, key, key, since, since, limit, offset)
}

// historyWhere keeps the history of a plugin key, of rollout id (0: any);
// it takes key, id, id.
const historyWhere = ` WHERE plugin_key=? AND (?::bigint=0 OR rollout_id=?)`

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
	if err := g.Raw(`SELECT count(*) FROM plugin_history`+historyWhere, key, rolloutID, rolloutID).Row().Scan(&total); err != nil {
		httpapi.Fail(c, err)
		return
	}
	items, err := store.CollectRows(g.Raw(`SELECT to_jsonb(h) FROM plugin_history h`+historyWhere+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		key, rolloutID, rolloutID, size, (page-1)*size), func(rows *sql.Rows) (json.RawMessage, error) {
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

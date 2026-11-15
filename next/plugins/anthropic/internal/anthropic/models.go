package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk"

	"github.com/Sub2API-Devs/sup2api/next/plugins/anthropic/internal/model"
)

// listModels serves GET /models: the model catalog from plg_anthropic.
// Rows are returned with every column (to_jsonb), so columns added by later
// migrations (e.g. "family" of the upgrade fixture) show up without code changes.
// Query: q (substring of model_id/display_name), status, page, page_size.
func (p *Plugin) listModels(ctx context.Context, req *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	if p.host == nil {
		return pluginsdk.ErrorResponse(http.StatusServiceUnavailable, "unavailable", "plugin not initialised"), nil
	}
	db, err := p.host.DB(ctx)
	if err != nil {
		return pluginsdk.ErrorResponse(http.StatusServiceUnavailable, "unavailable", "database unavailable: "+err.Error()), nil
	}
	page, size := pluginsdk.Pagination(req, 50)
	q := strings.TrimSpace(pluginsdk.Query(req, "q"))
	st := strings.TrimSpace(pluginsdk.Query(req, "status"))

	var total int64
	if err := catalog(db, q, st).Count(&total).Error; err != nil {
		return nil, err
	}
	// Scan leaves items as it is when no row comes back: [] in the JSON.
	items := []json.RawMessage{}
	if err := catalog(db, q, st).Select("to_jsonb(model_catalog)").Order("sort_order, model_id").
		Limit(size).Offset((page - 1) * size).Scan(&items).Error; err != nil {
		return nil, err
	}
	return pluginsdk.ListResponse(items, pluginsdk.Page{Page: page, PageSize: size, Total: total}), nil
}

// catalog selects the catalog rows matching q (a substring of model_id or
// display_name, as an ILIKE pattern) and status; empty matches everything.
func catalog(db *gorm.DB, q, status string) *gorm.DB {
	db = db.Model(&model.ModelCatalog{})
	if q != "" {
		db = db.Where("(model_id ILIKE '%' || ? || '%' OR display_name ILIKE '%' || ? || '%')", q, q)
	}
	if status != "" {
		db = db.Where("status = ?", status)
	}
	return db
}

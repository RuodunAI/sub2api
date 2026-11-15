package volcengine

// The local index over the Ark asset library (schema plg_volcengine, see
// migrations/0001_init.sql and internal/model). Upstream is the truth; these
// helpers only keep the index in step with what the last upstream call
// reported.
//
// They take the plugin's GORM handle bound to the request's context, or the
// transaction of withResource that holds the row's locks.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/pgdb"

	"github.com/Sub2API-Devs/sup2api/next/plugins/volcengine/internal/model"
)

// index status values of both tables.
const (
	IndexOK      = "ok"
	IndexMissing = "missing"
)

// ErrNoRow is returned when an index row does not exist.
var ErrNoRow = errors.New("not found in the local index")

// noRow turns GORM's missing row into ErrNoRow.
func noRow(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNoRow
	}
	return err
}

// groupRow is an asset_groups row, only the columns the plugin acts on. The
// console table gets the full row as JSON (to_jsonb), so a column added by a
// later migration shows up without touching this struct.
type groupRow struct {
	ID         int64
	AccountID  int64
	UpstreamID string
	Name       string
}

// assetRow is an assets row, only the columns the plugin acts on.
type assetRow struct {
	ID         int64
	AccountID  int64
	GroupID    int64
	UpstreamID string
}

func loadGroup(db *gorm.DB, id int64) (*groupRow, error) {
	var g groupRow
	err := db.Model(&model.AssetGroup{}).Select("id, account_id, upstream_id, name").Where("id = ?", id).Take(&g).Error
	if err != nil {
		return nil, noRow(err)
	}
	return &g, nil
}

func loadAsset(db *gorm.DB, id int64) (*assetRow, error) {
	var a assetRow
	err := db.Model(&model.Asset{}).Select("id, account_id, group_id, upstream_id").Where("id = ?", id).Take(&a).Error
	if err != nil {
		return nil, noRow(err)
	}
	return &a, nil
}

// insertGroup indexes a group that was just created upstream. It is called
// only after the upstream Action succeeded, so a conflict on
// (account_id, upstream_id) means the index already knew the group - which
// happens when a create is retried after a response that never arrived. The
// existing row wins and its id is returned, making the route idempotent
// instead of leaving a duplicate.
//
// Raw SQL: checked_at is the database's now(), which a GORM Create of a
// model cannot write, and the no-op DO UPDATE is what makes RETURNING
// answer the existing row's id.
func insertGroup(db *gorm.DB, accountID int64, g UpstreamGroup) (int64, error) {
	groupType := g.GroupType
	if groupType == "" {
		groupType = "AIGC"
	}
	var id int64
	err := db.Raw(`
		INSERT INTO asset_groups (account_id, upstream_id, name, title, description, group_type, index_status, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, 'ok', now())
		ON CONFLICT (account_id, upstream_id) DO UPDATE
		   SET upstream_id = asset_groups.upstream_id
		RETURNING id`,
		accountID, g.ID, g.Name, g.Title, g.Description, groupType).Row().Scan(&id)
	return id, err
}

// insertAsset indexes an asset created upstream, with the same idempotency
// rule as insertGroup.
func insertAsset(db *gorm.DB, accountID, groupID int64, a UpstreamAsset) (int64, error) {
	var id int64
	err := db.Raw(`
		INSERT INTO assets (account_id, group_id, upstream_id, name, asset_type, url, status, index_status, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'ok', now())
		ON CONFLICT (account_id, upstream_id) DO UPDATE
		   SET upstream_id = assets.upstream_id
		RETURNING id`,
		accountID, groupID, a.ID, a.Name, a.AssetType, a.URL, a.Status).Row().Scan(&id)
	return id, err
}

// checked is the SET of a write that finds the index row in step with
// upstream again; callers add the fields they write.
func checked() map[string]any {
	return map[string]any{"index_status": IndexOK, "updated_at": gorm.Expr("now()"), "checked_at": gorm.Expr("now()")}
}

// setNonEmpty adds the fields of an upstream result that carry a value.
func setNonEmpty(set map[string]any, fields map[string]string) map[string]any {
	for column, v := range fields {
		if v != "" {
			set[column] = v
		}
	}
	return set
}

// setSubmitted adds the fields an operator submitted (nil: not submitted).
func setSubmitted(set map[string]any, fields map[string]*string) map[string]any {
	for column, v := range fields {
		if v != nil {
			set[column] = *v
		}
	}
	return set
}

// refreshGroup writes back what a GetAssetGroup reported. Empty strings are
// kept out of the update: a result that does not carry a field must not blank
// the indexed value. (An UPDATE the operator asked for goes through
// applyGroupUpdate instead, which can blank a field on purpose.)
func refreshGroup(db *gorm.DB, id int64, g UpstreamGroup) error {
	return db.Model(&model.AssetGroup{}).Where("id = ?", id).Updates(setNonEmpty(checked(), map[string]string{
		"name": g.Name, "title": g.Title, "description": g.Description, "group_type": g.GroupType,
	})).Error
}

// applyGroupUpdate writes exactly the fields the operator submitted, so
// clearing a title upstream clears it in the index too. A nil pointer means
// "not submitted".
func applyGroupUpdate(db *gorm.DB, id int64, name, title, description *string) error {
	return db.Model(&model.AssetGroup{}).Where("id = ?", id).Updates(setSubmitted(checked(), map[string]*string{
		"name": name, "title": title, "description": description,
	})).Error
}

// refreshAsset writes back what a GetAsset reported.
func refreshAsset(db *gorm.DB, id int64, a UpstreamAsset) error {
	return db.Model(&model.Asset{}).Where("id = ?", id).Updates(setNonEmpty(checked(), map[string]string{
		"name": a.Name, "asset_type": a.AssetType, "url": a.URL, "status": a.Status,
	})).Error
}

// applyAssetUpdate writes exactly the fields the operator submitted.
func applyAssetUpdate(db *gorm.DB, id int64, name *string) error {
	return db.Model(&model.Asset{}).Where("id = ?", id).Updates(setSubmitted(checked(), map[string]*string{
		"name": name,
	})).Error
}

// indexRow is a row of the index tables, the only ones markMissing and
// deleteRow may touch.
type indexRow interface {
	model.AssetGroup | model.Asset
}

// markMissing records that the upstream copy of an index row is gone. The
// row is kept: an operator has to be able to see that the index and the
// Volcengine console disagree, and DELETE still cleans it up.
func markMissing[T indexRow](db *gorm.DB, id int64) error {
	return db.Model(new(T)).Where("id = ?", id).
		Updates(map[string]any{"index_status": IndexMissing, "checked_at": gorm.Expr("now()")}).Error
}

// deleteRow drops an index row.
func deleteRow[T indexRow](db *gorm.DB, id int64) error {
	return db.Where("id = ?", id).Delete(new(T)).Error
}

// ---------------------------------------------------------------- list queries

// listPage is one page of index rows, already JSON.
type listPage struct {
	Items []json.RawMessage
	Total int64
}

// LikeTerm turns a search box's text into the middle of an ILIKE pattern:
// the three characters LIKE treats as syntax are escaped so the term matches
// itself and nothing else.
//
// Without this, "%" in the box matches every row and "a_c" matches "abc" -
// a search that silently answers something other than what was asked, which
// is worse than a search that finds nothing. Backslash is escaped first
// because it is PostgreSQL's default LIKE escape character, which is also why
// no ESCAPE clause is needed at the call sites.
//
// An empty term is left empty: every query treats "" as "no filter" rather
// than as "matches everything", so the two must not be confused.
func LikeTerm(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	return strings.ReplaceAll(q, `_`, `\_`)
}

// The columns a free-text search looks at, per resource. Both lists hold only
// values an operator can SEE in the console table and would plausibly type:
// the name, a group's title, and the Ark id (which is what gets pasted in
// from a usage record or the Volcengine console). Deliberately not searched:
// an asset's url and a group's description, which are long, are not columns
// of either table, and would turn a name search into a haystack.
const (
	groupSearch = `(g.name ILIKE '%' || @q || '%' OR g.title ILIKE '%' || @q || '%' OR g.upstream_id ILIKE '%' || @q || '%')`
	assetSearch = `(a.name ILIKE '%' || @q || '%' OR a.upstream_id ILIKE '%' || @q || '%')`
)

// filter adds "column = value" unless value is the zero value (no filter).
func filter[T comparable](db *gorm.DB, column string, value T) *gorm.DB {
	var zero T
	if value == zero {
		return db
	}
	return db.Where(column+" = ?", value)
}

// search adds a free-text search (cond binds @q) unless q is empty.
func search(db *gorm.DB, cond, q string) *gorm.DB {
	if q == "" {
		return db
	}
	return db.Where(cond, sql.Named("q", q))
}

// listOf counts the rows rows selects and reads a page of them as the JSON
// items; Items is [] when the page is empty.
func listOf(rows *gorm.DB, items, order string, limit, offset int) (*listPage, error) {
	page, total, err := pgdb.List[json.RawMessage](rows, items, order, limit, offset)
	if err != nil {
		return nil, err
	}
	return &listPage{Items: page, Total: total}, nil
}

// listGroups returns a page of the group index, newest first, optionally for
// one account, one index status and/or a search term (already escaped by
// LikeTerm).
func listGroups(db *gorm.DB, accountID int64, status, q string, limit, offset int) (*listPage, error) {
	g := db.Table("asset_groups AS g")
	g = filter(g, "g.account_id", accountID)
	g = filter(g, "g.index_status", status)
	g = search(g, groupSearch, q)
	return listOf(g, `to_jsonb(g) || jsonb_build_object('asset_count',
		(SELECT count(*) FROM assets a WHERE a.group_id = g.id))`, "g.id DESC", limit, offset)
}

// listAssets returns a page of the asset index, newest first, optionally for
// one account, one local group, one index status and/or a search term
// (already escaped by LikeTerm).
func listAssets(db *gorm.DB, accountID, groupID int64, status, q string, limit, offset int) (*listPage, error) {
	a := db.Table("assets AS a").Joins("JOIN asset_groups g ON g.id = a.group_id")
	a = filter(a, "a.account_id", accountID)
	a = filter(a, "a.group_id", groupID)
	a = filter(a, "a.index_status", status)
	a = search(a, assetSearch, q)
	return listOf(a, `to_jsonb(a) || jsonb_build_object('group_name', g.name, 'group_upstream_id', g.upstream_id)`,
		"a.id DESC", limit, offset)
}

// withAccountNames adds account_name to every row, from the id -> name map
// the caller built with one ListAccounts pass. Accounts live in the core and
// the plugin role cannot join them, and denormalizing the name into the
// index would make it go stale on every rename - so it is merged in here, at
// read time.
//
// Numbers are decoded as json.Number so a bigint id survives the round trip
// unrounded.
func withAccountNames(items []json.RawMessage, names map[int64]string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			out = append(out, raw)
			continue
		}
		name := ""
		if n, ok := obj["account_id"].(json.Number); ok {
			if id, err := n.Int64(); err == nil {
				name = names[id]
			}
		}
		obj["account_name"] = name
		b, err := json.Marshal(obj)
		if err != nil {
			out = append(out, raw)
			continue
		}
		out = append(out, b)
	}
	return out
}

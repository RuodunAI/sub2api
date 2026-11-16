package usage

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

// Usage export (CONTRACTS §66.5): GET /me/usage/export, GET /usage/export
// and GET /account-usage/export (§102.5) take the list's filters plus tz=
// (the time column's zone) and lang=zh|en (the header row), and stream CSV
// newest first, keyset-paged so a long export never runs one large query. At most exportMaxRows rows; a longer
// result ends with a note row. Cells a spreadsheet would read as a formula
// are prefixed with '. The self-service export has no account or upstream
// columns, like the list; the account side's has its allowlisted columns.
// Every export is audited.

const (
	exportMaxRows = 100_000
	exportBatch   = 1000
)

type exportColumn struct {
	zh, en  string
	admin   bool // operators only
	account bool // also needs account:read (seesAccounts)
	value   func(r *Record, loc *time.Location) string
}

var exportColumns = []exportColumn{
	{"时间", "Time", false, false, func(r *Record, loc *time.Location) string { return r.CreatedAt.In(loc).Format("2006-01-02 15:04:05") }},
	{"请求 ID", "Request ID", false, false, func(r *Record, _ *time.Location) string { return r.RequestID }},
	{"客户端请求 ID", "Client request ID", false, false, func(r *Record, _ *time.Location) string { return r.ClientRequestID }},
	{"用户", "User", true, false, func(r *Record, _ *time.Location) string { return r.UserEmail }},
	{"API 密钥", "API key", false, false, func(r *Record, _ *time.Location) string { return r.APIKeyName }},
	{"分组", "Group", false, false, func(r *Record, _ *time.Location) string { return r.GroupName }},
	{"账号 ID", "Account ID", true, true, func(r *Record, _ *time.Location) string {
		if r.AccountID == nil {
			return ""
		}
		return strconv.FormatInt(*r.AccountID, 10)
	}},
	{"账号", "Account", true, true, func(r *Record, _ *time.Location) string { return r.AccountName }},
	{"模型", "Model", false, false, func(r *Record, _ *time.Location) string { return r.Model }},
	{"上游模型", "Upstream model", true, false, func(r *Record, _ *time.Location) string { return r.UpstreamModel }},
	{"端点", "Endpoint", false, false, func(r *Record, _ *time.Location) string { return r.Endpoint }},
	{"协议", "Protocol", false, false, func(r *Record, _ *time.Location) string { return r.Protocol }},
	{"流式", "Stream", false, false, func(r *Record, _ *time.Location) string { return strconv.FormatBool(r.Stream) }},
	{"状态码", "Status", false, false, func(r *Record, _ *time.Location) string { return strconv.Itoa(r.StatusCode) }},
	{"成功", "Success", false, false, func(r *Record, _ *time.Location) string { return strconv.FormatBool(r.Success) }},
	{"错误类型", "Error type", false, false, func(r *Record, _ *time.Location) string { return r.ErrorType }},
	{"输入 Token", "Input tokens", false, false, func(r *Record, _ *time.Location) string { return strconv.FormatInt(r.InputTokens, 10) }},
	{"输出 Token", "Output tokens", false, false, func(r *Record, _ *time.Location) string { return strconv.FormatInt(r.OutputTokens, 10) }},
	{"缓存读取 Token", "Cache read tokens", false, false, func(r *Record, _ *time.Location) string { return strconv.FormatInt(r.CacheReadTokens, 10) }},
	{"缓存写入 Token", "Cache write tokens", false, false, func(r *Record, _ *time.Location) string {
		return strconv.FormatInt(r.CacheCreationTokens+r.CacheCreation1hTokens, 10)
	}},
	{"费用", "Cost", false, false, func(r *Record, _ *time.Location) string { return r.TotalCost.String() }},
	{"原价", "Base price", false, false, func(r *Record, _ *time.Location) string {
		return r.StandardCost.Round(8).String() // a row from before base_cost is a quotient, with more digits
	}},
	{"倍率", "Multiplier", false, false, func(r *Record, _ *time.Location) string { return r.RateMultiplier.String() }},
	{"计费状态", "Billing status", false, false, func(r *Record, _ *time.Location) string { return r.BillingStatus }},
	{"首 token (ms)", "First token (ms)", false, false, func(r *Record, _ *time.Location) string { return strconv.Itoa(r.FirstTokenMs) }},
	{"耗时 (ms)", "Duration (ms)", false, false, func(r *Record, _ *time.Location) string { return strconv.Itoa(r.LatencyMs) }},
	{"调用 IP", "Client IP", false, false, func(r *Record, _ *time.Location) string { return r.ClientIP }},
}

// attemptChain is the call chain cell of a retried request (CONTRACTS §98):
// the accounts its attempts went to in order, consecutive attempts on one
// account merged ("#16×2 → #12"), numbered instead where the account ids
// are hidden ("Account 1×2 → Account 2"). The owner of the request, and a
// row from before the trail, get the attempt count. A request sent once
// gets nothing.
func attemptChain(r *Record, zh bool) string {
	if r.Attempts < 2 {
		return ""
	}
	var tr core.AttemptTrail
	if json.Unmarshal(r.AttemptTrail, &tr) != nil || len(tr.Steps) == 0 || tr.Steps[0].AccountID == 0 && tr.Steps[0].AccountRef == 0 {
		if zh {
			return strconv.Itoa(r.Attempts) + " 次"
		}
		return strconv.Itoa(r.Attempts) + " attempts"
	}
	ref := "Account "
	if zh {
		ref = "账号"
	}
	var parts []string
	for i := 0; i < len(tr.Steps); {
		first, n := tr.Steps[i], 1
		for i+n < len(tr.Steps) && tr.Steps[i+n].AccountID == first.AccountID && tr.Steps[i+n].AccountRef == first.AccountRef {
			n++
		}
		part := "#" + strconv.FormatInt(first.AccountID, 10)
		if first.AccountID == 0 {
			part = ref + strconv.Itoa(first.AccountRef)
		}
		if n > 1 {
			part += "×" + strconv.Itoa(n)
		}
		parts = append(parts, part)
		i += n
	}
	if tr.Omitted > 0 {
		parts = append(parts, "+"+strconv.Itoa(tr.Omitted))
	}
	return strings.Join(parts, " → ")
}

func (s *Service) myExport(c *gin.Context) {
	uid, _ := core.UserID(c.Request.Context())
	s.export(c, &uid)
}

func (s *Service) allExport(c *gin.Context) { s.export(c, nil) }

func (s *Service) export(c *gin.Context, self *int64) {
	accounts, err := s.seesAccounts(c.Request.Context(), self)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	view := trailViewFor(self, accounts)
	var cols []exportColumn
	for _, col := range exportColumns {
		if (!col.admin || self == nil) && (!col.account || accounts) {
			cols = append(cols, col)
		}
	}
	s.streamExport(c, csvExport{
		filter: func() (store.Conds, error) { return parseFilter(c, self, view) },
		cols:   cols,
		shape: func(rs []Record) error {
			for i := range rs {
				if self != nil {
					rs[i].hideUpstream()
				}
				rs[i].shapeTrail(view, nil)
			}
			return nil
		},
		audit: map[string]any{"self": self != nil},
	})
}

// accountExportColumns are the columns of the account side's export
// (CONTRACTS §102.5), by their en header: the allowlisted fields alone.
var accountExportColumns = []string{"Time", "Request ID", "Account ID", "Account", "Model", "Upstream model", "Endpoint",
	"Protocol", "Stream", "Status", "Success", "Error type", "Input tokens", "Output tokens", "Cache read tokens",
	"Cache write tokens", "Base price", "First token (ms)", "Duration (ms)"}

// csvExport is what one export streams: the records filter keeps (parsed
// again for each page), the columns before the call chain, what is left of
// each page's records (shape), and the audit detail beyond rows,
// truncated and query.
type csvExport struct {
	filter func() (store.Conds, error)
	cols   []exportColumn
	shape  func([]Record) error
	audit  map[string]any
}

func (s *Service) streamExport(c *gin.Context, e csvExport) {
	ctx := c.Request.Context()
	// Validate the filters before any byte of the file is sent; each page
	// below parses them again and adds its keyset bound.
	if _, err := e.filter(); err != nil {
		httpapi.Fail(c, err)
		return
	}
	_, loc, err := parseTZ(c)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	lang := c.Query("lang")
	if lang == "" {
		lang = "en"
		if strings.HasPrefix(strings.ToLower(c.GetHeader("Accept-Language")), "zh") {
			lang = "zh"
		}
	}
	if lang != "zh" && lang != "en" {
		httpapi.Fail(c, core.ErrInvalidArgument.WithMessage("lang must be zh or en"))
		return
	}
	// The call chain comes last, in the header's language.
	cols := append(slices.Clip(e.cols), exportColumn{zh: "调用链路", en: "Call chain", value: func(r *Record, _ *time.Location) string {
		return attemptChain(r, lang == "zh")
	}})
	name := "usage_" + time.Now().In(loc).Format("20060102_150405") + ".csv"
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM: spreadsheets then read the file as UTF-8
	w := csv.NewWriter(c.Writer)
	head := make([]string, len(cols))
	for i, col := range cols {
		head[i] = col.en
		if lang == "zh" {
			head[i] = col.zh
		}
	}
	_ = w.Write(head)

	rows, truncated := 0, false
	var cursorAt time.Time
	var cursorID int64
	for rows < exportMaxRows {
		page, err := e.filter()
		if err != nil {
			break
		}
		if cursorID != 0 {
			page.And("(u.created_at, u.id) < (?, ?)", cursorAt, cursorID)
		}
		batch := min(exportBatch, exportMaxRows-rows)
		records, err := scanRecords(recordQuery(store.G(ctx, s.db.Gorm), page).Limit(batch + 1))
		more := len(records) > batch
		if more {
			records = records[:batch]
		}
		if err == nil {
			err = e.shape(records)
		}
		if err != nil {
			// Headers are sent: end the file with the error so it is not
			// mistaken for a complete export.
			_ = w.Write([]string{"# export failed: " + err.Error()})
			break
		}
		for i := range records {
			line := make([]string, len(cols))
			for j, col := range cols {
				line[j] = csvCell(col.value(&records[i], loc))
			}
			_ = w.Write(line)
		}
		if n := len(records); n > 0 {
			rows += n
			cursorAt, cursorID = records[n-1].CreatedAt, records[n-1].ID
		}
		w.Flush()
		if !more {
			break
		}
		if rows >= exportMaxRows {
			truncated = true
		}
	}
	if truncated {
		_ = w.Write([]string{fmt.Sprintf("# truncated at %d rows; narrow the time range or filters", exportMaxRows)})
	}
	w.Flush()
	uid, _ := core.UserID(ctx)
	detail := map[string]any{"rows": rows, "truncated": truncated, "query": c.Request.URL.RawQuery}
	maps.Copy(detail, e.audit)
	raw, _ := json.Marshal(detail)
	_ = store.G(ctx, s.db.Gorm).Create(&model.AuditLog{UserID: &uid, Action: "usage.export", TargetType: "usage", Detail: raw, IP: c.ClientIP()}).Error
}

// csvCell keeps a spreadsheet from evaluating a text cell as a formula.
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

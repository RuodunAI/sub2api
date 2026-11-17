package notify

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

func (s *Service) RegisterRoutes(r *httpapi.Router) {
	r.Perm("GET", "/notifications/channels", "settings:read", s.listChannels)
	r.Perm("GET", "/notifications", "settings:read", s.list)
	r.Perm("POST", "/notifications/test", "settings:manage", s.test)
}

// ChannelView is a channel as the console shows it.
type ChannelView struct {
	ID          string                 `json:"id"`
	PluginKey   string                 `json:"plugin_key"`
	PluginName  manifest.LocalizedText `json:"plugin_name"`
	Label       manifest.LocalizedText `json:"label"`
	Description manifest.LocalizedText `json:"description"`
	Recipient   string                 `json:"recipient"`
}

func (s *Service) listChannels(c *gin.Context) {
	out := []ChannelView{}
	for _, b := range s.NotificationChannels() {
		out = append(out, ChannelView{ID: b.Channel.ID, PluginKey: b.Plugin.Key, PluginName: pluginName(b.Plugin),
			Label: b.Channel.Label, Description: b.Channel.Description, Recipient: b.Channel.RecipientFormat()})
	}
	httpapi.OK(c, out)
}

// Record is one row of GET /notifications.
type Record struct {
	ID            int64      `json:"id"`
	Source        string     `json:"source"`
	Channel       string     `json:"channel"`
	Recipients    []string   `json:"recipients"`
	Subject       string     `json:"subject"`
	Body          string     `json:"body"`
	Severity      string     `json:"severity"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at"`
}

// list is GET /notifications?status=&source=, newest first.
func (s *Service) list(c *gin.Context) {
	ctx := c.Request.Context()
	page, size := httpapi.Pagination(c)
	status, source := c.Query("status"), c.Query("source")
	db := store.G(ctx, s.db.Gorm)
	var total int64
	if err := db.Model(&model.Notification{}).Scopes(filterRecords(status, source)).Count(&total).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	var rows []model.Notification
	if err := recordPage(db, status, source, size, (page-1)*size, &rows).Error; err != nil {
		httpapi.Fail(c, err)
		return
	}
	items := make([]Record, 0, len(rows))
	for _, r := range rows {
		items = append(items, recordOf(r))
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
}

// recordOf is row r as the console shows it.
func recordOf(r model.Notification) Record {
	return Record{ID: r.ID, Source: r.Source, Channel: r.Channel, Recipients: r.Recipients, Subject: r.Subject,
		Body: r.Body, Severity: core.ValueOr(r.Severity, ""), Status: core.ValueOr(r.Status, ""), Attempts: r.Attempts, LastError: r.LastError,
		NextAttemptAt: r.NextAttemptAt, CreatedAt: r.CreatedAt, SentAt: r.SentAt}
}

// recordColumns are the columns of a Record.
const recordColumns = "id, source, channel, recipients, subject, body, severity, status, attempts, last_error, next_attempt_at, created_at, sent_at"

// filterRecords keeps the rows of status and source; an empty value does
// not filter.
func filterRecords(status, source string) func(*gorm.DB) *gorm.DB {
	var f store.Conds
	if status != "" {
		f.And("status = ?", status)
	}
	if source != "" {
		f.And("source = ?", source)
	}
	return f.Scope()
}

// recordPage reads one page of the filtered rows into dst, newest first.
func recordPage(db *gorm.DB, status, source string, limit, offset int, dst *[]model.Notification) *gorm.DB {
	return db.Select(recordColumns).Scopes(filterRecords(status, source)).Order("id DESC").Limit(limit).Offset(offset).Find(dst)
}

// sourceTest is the source of console tests, so they do not count toward
// the rate limit of the core's own notifications.
const sourceTest = core.NotificationSourceCore + ":test"

// test is POST /notifications/test {channel, recipients}: a test message
// from the core, to check a channel's configuration. It answers with the
// Record once the first attempt is recorded, or as it is after TestWait
// (awaitAttempt), so the console shows whether the channel works.
func (s *Service) test(c *gin.Context) {
	var in struct {
		Channel    string   `json:"channel"`
		Recipients []string `json:"recipients"`
	}
	if !httpapi.BindJSON(c, &in) {
		return
	}
	ctx := c.Request.Context()
	r, err := s.Notify(ctx, sourceTest, core.Notification{Channel: in.Channel, Recipients: in.Recipients,
		Subject: core.Text(ctx, "Sub2API test notification", "Sub2API 测试通知"),
		Body:    core.Text(ctx, "This is a test notification. The channel works.", "这是一条测试通知，说明该渠道可以正常发送。")})
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	rec, err := s.awaitAttempt(ctx, r.ID)
	if err != nil {
		httpapi.Fail(c, err)
		return
	}
	httpapi.OK(c, rec)
}

// attemptCheck is how often awaitAttempt reads the row.
const attemptCheck = 250 * time.Millisecond

// awaitAttempt reads row id until its first attempt is recorded or TestWait
// passes, and returns it as it is then. The row is delivered by the worker
// like any other (whichever node claims it, with its lease, retries and
// outcome rules); this only reads the record.
func (s *Service) awaitAttempt(ctx context.Context, id int64) (Record, error) {
	deadline := time.Now().Add(s.opts.TestWait)
	for {
		var row model.Notification
		if err := store.G(ctx, s.db.Gorm).Select(recordColumns).Take(&row, id).Error; err != nil {
			return Record{}, err
		}
		if attempted(row) || time.Now().After(deadline) {
			return recordOf(row), nil
		}
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-time.After(attemptCheck):
		}
	}
}

// attempted reports whether an attempt at a new row has been recorded: it
// was sent or failed, or it waits for a retry with the attempt's error.
func attempted(r model.Notification) bool {
	switch core.ValueOr(r.Status, "") {
	case "sent", "failed":
		return true
	case "pending":
		return r.LastError != ""
	}
	return false
}

func pluginName(p core.PluginInfo) manifest.LocalizedText {
	if p.Manifest != nil && len(p.Manifest.Name) > 0 {
		return p.Manifest.Name
	}
	return manifest.LocalizedText{"en": p.Key}
}

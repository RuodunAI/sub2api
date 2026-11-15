package notify

import (
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
	db := store.G(ctx, s.db.Pool)
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
		items = append(items, Record{ID: r.ID, Source: r.Source, Channel: r.Channel, Recipients: r.Recipients, Subject: r.Subject,
			Body: r.Body, Severity: core.ValueOr(r.Severity, ""), Status: core.ValueOr(r.Status, ""), Attempts: r.Attempts, LastError: r.LastError,
			NextAttemptAt: r.NextAttemptAt, CreatedAt: r.CreatedAt, SentAt: r.SentAt})
	}
	httpapi.List(c, items, httpapi.Page{Page: page, PageSize: size, Total: total})
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
// from the core, to check a channel's configuration.
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
	httpapi.OK(c, gin.H{"id": r.ID})
}

func pluginName(p core.PluginInfo) manifest.LocalizedText {
	if p.Manifest != nil && len(p.Manifest.Name) > 0 {
		return p.Manifest.Name
	}
	return manifest.LocalizedText{"en": p.Key}
}

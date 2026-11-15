package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/textutil"

	"github.com/Sub2API-Devs/sup2api/next/plugins/guard/internal/model"
)

// DenyCode is returned to clients when a rule blocks a request.
const DenyCode = "guard_blocked"

const snippetRadius = 60

// blockEvent is one blocked request, persisted asynchronously.
type blockEvent struct {
	At        time.Time
	RuleID    int64
	RuleName  string
	RequestID string
	UserID    int64
	GroupID   int64
	Model     string
	Snippet   string
}

// alert is one webhook notification. Cooldown is the throttling window in
// force when the request was blocked (0 = not throttled).
type alert struct {
	URL      string
	Event    blockEvent
	Cooldown time.Duration
}

// fieldString decodes a hook field: JSON string when possible, raw otherwise.
func fieldString(raw string) string {
	if strings.HasPrefix(raw, `"`) {
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			return s
		}
	}
	return raw
}

// snippetAround returns up to snippetRadius bytes around offset, on rune
// boundaries.
func snippetAround(text string, offset, length int) string {
	start := offset - snippetRadius
	if start < 0 {
		start = 0
	}
	end := offset + length + snippetRadius
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && !isRuneStart(text[start]) {
		start--
	}
	for end < len(text) && !isRuneStart(text[end]) {
		end++
	}
	return text[start:end]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// OnGatewayRequest implements pluginsdk.Hook.
func (p *Plugin) OnGatewayRequest(_ context.Context, in *pluginv1.GatewayRequestHookRequest) (*pluginv1.GatewayRequestHookResponse, error) {
	p.stats.checked.Add(1)
	text := fieldString(in.GetFields()["prompt_text"])
	allow := &pluginv1.GatewayRequestHookResponse{Decision: pluginv1.GatewayRequestHookResponse_DECISION_ALLOW}
	if text == "" {
		return allow, nil
	}
	lower := strings.ToLower(text)
	for _, r := range p.rules.Load().rules {
		off := r.match(text, lower)
		if off < 0 {
			continue
		}
		p.stats.blocked.Add(1)
		meta := in.GetMeta()
		model := fieldString(in.GetFields()["model"])
		if model == "" {
			model = meta.GetModel()
		}
		ev := blockEvent{
			At: p.now().UTC(), RuleID: r.ID, RuleName: r.Name, RequestID: meta.GetRequestId(),
			UserID: meta.GetUserId(), GroupID: meta.GetGroupId(), Model: model,
		}
		settings := p.settings.Load()
		if settings.RecordSnippets {
			ev.Snippet = snippetAround(text, off, len(r.Pattern))
		}
		if !p.blockWriter.Send(ev) {
			p.stats.droppedBlocks.Add(1)
		}
		if settings.WebhookURL != "" {
			// Throttling is decided in alertSender (it may call the host);
			// here only the in-memory check that drops alerts this node
			// already knows cannot go out.
			cooldown := settings.alertCooldown()
			if cooldown > 0 && p.throttle.cooling(r.ID, ev.At, cooldown) {
				p.stats.alertsThrottled.Add(1)
			} else {
				select {
				case p.alerts <- alert{URL: settings.WebhookURL, Event: ev, Cooldown: cooldown}:
				default:
					p.stats.droppedAlerts.Add(1)
				}
			}
		}
		return &pluginv1.GatewayRequestHookResponse{
			Decision:    pluginv1.GatewayRequestHookResponse_DECISION_DENY,
			DenyStatus:  http.StatusForbidden,
			DenyCode:    DenyCode,
			DenyMessage: fmt.Sprintf("Request blocked by guard rule %q / 请求被拦截规则「%s」拒绝", r.Name, r.Name),
			Note:        fmt.Sprintf("guard: rule %d (%s)", r.ID, textutil.TruncateRunes(r.Name, 60)),
		}, nil
	}
	return allow, nil
}

func (p *Plugin) writeBlocks(ctx context.Context, evs []blockEvent) error {
	db, err := p.db(ctx)
	if err != nil {
		return err
	}
	logs := make([]model.BlockLog, len(evs))
	// One counter row per minute, group and model (an INSERT cannot update
	// a row twice), in the order the events first touch them: the order the
	// one statement per event this replaces locked them in.
	type key struct {
		minute, group int64
		model         string
	}
	var counts []model.StatsMinutely
	index := map[key]int{}
	for i, ev := range evs {
		logs[i] = model.BlockLog{OccurredAt: ev.At, RuleID: ev.RuleID, RuleName: ev.RuleName, RequestID: ev.RequestID,
			UserID: ev.UserID, GroupID: ev.GroupID, Model: ev.Model}
		if ev.Snippet != "" {
			logs[i].Snippet = &ev.Snippet
		}
		minute := minuteOf(ev.At)
		k := key{minute.Unix(), ev.GroupID, ev.Model}
		j, ok := index[k]
		if !ok {
			j, index[k] = len(counts), len(counts)
			counts = append(counts, model.StatsMinutely{Minute: minute, GroupID: ev.GroupID, Model: ev.Model})
		}
		counts[j].Blocked++
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.CreateInBatches(&logs, insertBatch).Error; err != nil {
			return err
		}
		return addStats(tx, counts)
	})
}

// alertSender posts webhook alerts with plain http.Post semantics (the
// default transport goes through the egress tunnel in strict mode). Alerts
// with a cooldown are sent only when claimAlert allows it; the others are
// counted as throttled. A failed POST still uses up the window.
func (p *Plugin) alertSender(ctx context.Context) {
	defer p.wg.Done()
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-p.alerts:
			if a.Cooldown > 0 && !p.claimAlert(ctx, a.Event.RuleID, a.Cooldown) {
				p.stats.alertsThrottled.Add(1)
				continue
			}
			body, _ := json.Marshal(map[string]any{
				"plugin":       "guard",
				"event":        "request.blocked",
				"rule_id":      a.Event.RuleID,
				"rule_name":    a.Event.RuleName,
				"request_id":   a.Event.RequestID,
				"user_id":      a.Event.UserID,
				"group_id":     a.Event.GroupID,
				"model":        a.Event.Model,
				"snippet":      a.Event.Snippet,
				"at":           a.Event.At.Format(time.RFC3339),
				"cooldown_sec": int64(a.Cooldown / time.Second),
			})
			resp, err := client.Post(a.URL, "application/json", bytes.NewReader(body))
			if err != nil {
				p.stats.alertErrors.Add(1)
				p.log.Warn("guard: webhook failed", "error", err.Error())
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode >= 300 {
				p.stats.alertErrors.Add(1)
				p.log.Warn("guard: webhook rejected", "status", resp.StatusCode)
			}
		}
	}
}

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// gatewayWS opens the OpenAI Responses WebSocket of the gateway (base: the
// load balancer or a node) with an API key.
func gatewayWS(ctx context.Context, t testing.TB, base, apiKey string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v1/responses",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + apiKey}}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket handshake: HTTP %d %v", status, err)
	}
	conn.SetReadLimit(64 << 20)
	return conn
}

// wsTurn sends one response.create and reads its events up to the terminal
// one (response.completed, response.failed or error).
func wsTurn(ctx context.Context, t testing.TB, conn *websocket.Conn, msg map[string]any) []gjson.Result {
	t.Helper()
	raw, _ := json.Marshal(msg)
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("websocket write: %v", err)
	}
	var events []gjson.Result
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("websocket read after %d events: %v", len(events), err)
		}
		ev := gjson.ParseBytes(data)
		events = append(events, ev)
		switch ev.Get("type").String() {
		case "response.completed", "response.failed", "error":
			return events
		}
	}
}

// AC 34: the WebSocket mode of OpenAI Responses goes turn by turn through
// Execute: the plugin's first turn opens the upstream connection (one
// handshake, with the account's credential), every turn's message is the
// one the plugin built, and every turn is billed. openai/apikey and codex
// (whose plugin adds store=false and its own headers) both.
func TestAC34_ResponsesWebSocketTurns(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	m := e.Mock()
	for _, c := range []struct {
		name, plugin, typ, host, path string
		creds                         func(k string) map[string]any
	}{
		{name: "openai", plugin: OpenAIPlugin, typ: APIKeyType, path: "/v1/responses",
			creds: func(k string) map[string]any { return map[string]any{"api_key": k, "base_url": e.MockInternalURL} }},
		{name: "codex", plugin: "codex", typ: "codex_oauth", host: "chatgpt.com", path: "/backend-api/codex/responses",
			creds: func(k string) map[string]any {
				return map[string]any{"access_token": k, "chatgpt_account_id": "acct-e2e"}
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.With(t)
			e.EnsurePlugin(admin, c.plugin, "")
			st := e.newSingleTenant(admin, "grp-ws-"+c.name)
			k := fmt.Sprintf("sk-ws-%s-%d", e.RunID, nextSeq())
			id := e.CreateAccount(admin, AccountSpec{PluginKey: c.plugin, Type: c.typ, GroupIDs: []int64{st.GroupID}, Credentials: c.creds(k)})
			model := e.RunModelOf(admin, "gpt", "ws-"+c.name)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			mark := m.Mark(t)
			conn := gatewayWS(ctx, t, e.BaseURL, st.APIKey)
			defer conn.CloseNow()
			prompts := []string{"turn-one-" + e.RunID, "turn-two-" + e.RunID}
			for i, p := range prompts {
				events := wsTurn(ctx, t, conn, map[string]any{"type": "response.create", "model": model, "input": p})
				last := events[len(events)-1]
				if last.Get("type").String() != "response.completed" || last.Get("response.usage.output_tokens").Int() != MockOutputTokens {
					t.Fatalf("turn %d: %v", i, events)
				}
			}
			conn.Close(websocket.StatusNormalClosure, "")
			calls := mockCallsOf(t, m, mark, k)
			var handshakes, turns []MockRequest
			for _, r := range calls {
				switch r.Method {
				case http.MethodGet:
					handshakes = append(handshakes, r)
				case "WS":
					turns = append(turns, r)
				}
			}
			if len(handshakes) != 1 || len(turns) != 2 || handshakes[0].Path != c.path || (c.host != "" && handshakes[0].Host != c.host) {
				t.Fatalf("upstream: %d handshakes, %d turns, want 1 and 2 on %s%s: %+v", len(handshakes), len(turns), c.host, c.path, calls)
			}
			if handshakes[0].Header("authorization") != "Bearer "+k {
				t.Fatalf("handshake credential: %s", handshakes[0].Headers.Raw)
			}
			for i, r := range turns {
				if !strings.Contains(r.Body, prompts[i]) || gjson.Get(r.Body, "model").String() != model || gjson.Get(r.Body, "type").String() != "response.create" {
					t.Fatalf("turn %d message: %s", i, r.Body)
				}
				if c.name == "codex" && gjson.Get(r.Body, "store").Raw != "false" {
					t.Fatalf("codex turn %d message without store=false: %s", i, r.Body)
				}
			}
			// One billed usage record per turn.
			var billed []gjson.Result
			Eventually(t, 30*time.Second, time.Second, "two billed websocket turns", func() bool {
				billed = billed[:0]
				for _, u := range admin.ListAll(t, "/usage", "user_id", fmt.Sprint(st.User.UserID)) {
					if u.Get("account_id").Int() == id && u.Get("billing_status").String() == "billed" {
						billed = append(billed, u)
					}
				}
				return len(billed) == 2
			})
			for _, u := range billed {
				if u.Get("output_tokens").Int() != MockOutputTokens || Money(t, u.Get("total_cost").String()).Sign() <= 0 {
					t.Fatalf("websocket turn usage: %s", u.Raw)
				}
			}
		})
	}
}

// AC 35: the request body limit is the gateway setting max_request_body_mb
// alone: a request over it is refused with 413 (a WebSocket message over it
// closes the connection with 1009) and nothing reaches the upstream;
// raising the setting takes effect without a restart.
func TestAC35_RequestBodyLimitSetting(t *testing.T) {
	e := Setup(t)
	admin := e.Admin()
	m := e.Mock()
	tn := e.NewTenant(admin, TenantOpts{})
	st := e.newSingleTenant(admin, "grp-limit-ws")
	wsKey := fmt.Sprintf("sk-limit-%s-%d", e.RunID, nextSeq())
	e.CreateAccount(admin, AccountSpec{PluginKey: OpenAIPlugin, Type: APIKeyType, GroupIDs: []int64{st.GroupID}, APIKey: wsKey})
	gptModel := e.RunModelOf(admin, "gpt", "limit")

	prev := e.setMaxRequestBodyMB(admin, 1)
	t.Cleanup(func() { e.setMaxRequestBodyMB(admin, prev) })
	big := strings.Repeat("x", 1536<<10) // 1.5 MiB of prompt
	body := MessagesBody(tn.Model, big, false)

	// Every node applies the new value (cached for up to 10 s otherwise);
	// until then a node serves the request, so the upstream is watched only
	// from here on.
	Eventually(t, 20*time.Second, time.Second, "413 on every node", func() bool {
		for _, base := range e.NodeURLs {
			if g := e.Gateway(t, base, "/v1/messages", tn.APIKey, body, nil); g.Status != http.StatusRequestEntityTooLarge {
				return false
			}
		}
		return true
	})
	mark := m.Mark(t)
	g := e.Messages(tn.APIKey, body, nil)
	if g.Status != http.StatusRequestEntityTooLarge || g.JSON().Get("error.type").String() == "" {
		t.Fatalf("over the limit: HTTP %d %s", g.Status, g.Body)
	}
	for _, k := range KeysUsed(m.Since(t, mark), "") {
		if tn.AccountKeys()[k] {
			t.Fatal("a request over the body limit reached the upstream")
		}
	}

	// A WebSocket message over the limit closes the connection with 1009.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := gatewayWS(ctx, t, e.NodeURLs[0], st.APIKey)
	raw, _ := json.Marshal(map[string]any{"type": "response.create", "model": gptModel, "input": big})
	_ = conn.Write(ctx, websocket.MessageText, raw)
	_, _, err := conn.Read(ctx)
	if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
		t.Fatalf("websocket message over the limit: read error %v (close status %d), want 1009", err, status)
	}
	conn.CloseNow()

	// Raised to 2 MB: the same request is served.
	e.setMaxRequestBodyMB(admin, 2)
	Eventually(t, 20*time.Second, time.Second, "served on every node after raising the limit", func() bool {
		for _, base := range e.NodeURLs {
			if g := e.Gateway(t, base, "/v1/messages", tn.APIKey, body, nil); g.Status != 200 {
				return false
			}
		}
		return true
	})
	conn = gatewayWS(ctx, t, e.NodeURLs[0], st.APIKey)
	defer conn.CloseNow()
	events := wsTurn(ctx, t, conn, map[string]any{"type": "response.create", "model": gptModel, "input": big})
	if events[len(events)-1].Get("type").String() != "response.completed" {
		t.Fatalf("websocket message under the raised limit: %v", events)
	}
}

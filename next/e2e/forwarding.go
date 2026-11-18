package e2e

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Forwarding v2 helpers (docs/DESIGN-2026-11-18-PLUGIN-FORWARDING-V2.md):
// every platform plugin builds the final upstream request in Execute and the
// core sends it, so what the mock records is exactly what the plugin built,
// after the operator's header settings.

// Usage family of a protocol: how the mock's DefaultUsage shows in the
// usage record and what it costs at RunPrice.
type usageFamily int

const (
	usageAnthropic usageFamily = iota // exclusive input, cache read and creation
	usageOpenAI                       // inclusive input of which cached
	usageGemini                       // inclusive input, output + thoughts
)

// expectBilled checks the usage record of requestID: the account, billed at
// RunPrice from the mock's default usage of the family, the stream flag,
// and one usage debit equal to the cost in the ledger.
func (e *Env) expectBilled(admin *Session, userID int64, requestID string, accountID int64, fam usageFamily, stream bool) gjson.Result {
	e.T.Helper()
	u := e.UsageByRequest(admin, userID, requestID)
	if u.Get("account_id").Int() != accountID || u.Get("billing_status").String() != "billed" || !u.Get("success").Bool() || u.Get("stream").Bool() != stream {
		e.T.Fatalf("usage of %s: want billed, account %d, stream %v: %s", requestID, accountID, stream, u.Raw)
	}
	var want *big.Rat
	switch fam {
	case usageAnthropic:
		if u.Get("input_tokens").Int() != MockInputTokens || u.Get("output_tokens").Int() != MockOutputTokens ||
			u.Get("cache_read_tokens").Int() != MockCacheReadTokens || u.Get("cache_creation_tokens").Int() != MockCacheCreationTokens {
			e.T.Fatalf("usage tokens (anthropic): %s", u.Raw)
		}
		want = ExpectedTokenCost(RunPrice, MockInputTokens, MockOutputTokens, MockCacheReadTokens, MockCacheCreationTokens, 0, 1)
	case usageOpenAI:
		if u.Get("input_tokens").Int() != MockInputTokens || u.Get("output_tokens").Int() != MockOutputTokens || u.Get("cache_read_tokens").Int() != MockCacheReadTokens {
			e.T.Fatalf("usage tokens (openai): %s", u.Raw)
		}
		want = ExpectedTokenCost(RunPrice, MockInputTokens-MockCacheReadTokens, MockOutputTokens, MockCacheReadTokens, 0, 0, 1)
	case usageGemini:
		out := int64(MockOutputTokens + MockThoughtsTokens)
		if u.Get("input_tokens").Int() != MockInputTokens || u.Get("output_tokens").Int() != out || u.Get("cache_read_tokens").Int() != MockCacheReadTokens {
			e.T.Fatalf("usage tokens (gemini): %s", u.Raw)
		}
		want = ExpectedTokenCost(RunPrice, MockInputTokens-MockCacheReadTokens, out, MockCacheReadTokens, 0, 0, 1)
	}
	AssertMoney(e.T, "usage.total_cost", u.Get("total_cost").String(), want)
	e.expectOneDebit(admin, userID, requestID, want)
	return u
}

// expectOneDebit requires exactly one settled usage debit of the request,
// of amount want (a request billed once whatever the attempts).
func (e *Env) expectOneDebit(admin *Session, userID int64, requestID string, want *big.Rat) {
	e.T.Helper()
	var rows []gjson.Result
	for _, l := range e.UsageDebits(admin, userID) {
		if l.Get("ref_id").String() == requestID {
			rows = append(rows, l)
		}
	}
	if len(rows) != 1 {
		e.T.Fatalf("usage debits of %s = %d, want 1: %v", requestID, len(rows), rows)
	}
	AssertMoney(e.T, "ledger debit", new(big.Rat).Neg(Money(e.T, rows[0].Get("delta").String())).FloatString(8), want)
}

// mockCallsOf returns the mock requests since mark carrying upstream key.
func mockCallsOf(t testing.TB, m *Mock, mark int64, key string) []MockRequest {
	t.Helper()
	var out []MockRequest
	for _, r := range m.Since(t, mark) {
		if r.Key() == key || strings.Contains(r.Header("authorization"), key) {
			out = append(out, r)
		}
	}
	return out
}

// oneMockCall requires exactly one mock request with key since mark.
func oneMockCall(t testing.TB, m *Mock, mark int64, key, what string) MockRequest {
	t.Helper()
	calls := mockCallsOf(t, m, mark, key)
	if len(calls) != 1 {
		t.Fatalf("%s: upstream calls with the account's key = %d, want 1: %+v", what, len(calls), calls)
	}
	return calls[0]
}

// validJSONBody requires the recorded upstream body to be one JSON object
// that names model (when model is not "") and carries the prompt.
func validJSONBody(t testing.TB, r MockRequest, model, prompt, what string) {
	t.Helper()
	if !json.Valid([]byte(r.Body)) || !gjson.Parse(r.Body).IsObject() {
		t.Fatalf("%s: upstream body is not a JSON object: %q", what, r.Body)
	}
	if model != "" && gjson.Get(r.Body, "model").String() != model {
		t.Fatalf("%s: upstream body model = %q, want %q: %s", what, gjson.Get(r.Body, "model").String(), model, r.Body)
	}
	if prompt != "" && !strings.Contains(r.Body, prompt) {
		t.Fatalf("%s: upstream body lacks the prompt %q: %s", what, prompt, r.Body)
	}
}

// singleTenant is one user with balance, one group holding the given
// accounts and a key of that group.
type singleTenant struct {
	GroupID int64
	User    *Session
	APIKey  string
}

// newSingleTenant creates the group, the user (credit "20") and the key;
// accounts are created by the caller with GroupIDs {GroupID}.
func (e *Env) newSingleTenant(admin *Session, name string) *singleTenant {
	e.T.Helper()
	st := &singleTenant{GroupID: e.CreateGroup(admin, e.Name(name), "restricted", 1, nil)}
	st.User = e.CreateUser(admin, UserSpec{})
	e.GrantGroups(admin, st.User.UserID, []int64{st.GroupID})
	e.AdjustBalance(admin, st.User.UserID, "20", true, "e2e forwarding credit")
	_, st.APIKey = e.CreateAPIKey(st.User, st.GroupID)
	return st
}

// enableProtocols PATCHes an account's model_protocols: model served under
// protocols only (list the native one too).
func (e *Env) enableProtocols(admin *Session, accountID int64, model string, protocols ...string) {
	e.T.Helper()
	admin.OK(e.T, http.MethodPatch, fmt.Sprintf("/accounts/%d", accountID), map[string]any{"model_protocols": []any{
		map[string]any{"models": []string{model}, "protocols": protocols}}})
}

// setMaxRequestBodyMB sets the gateway setting max_request_body_mb (PUT
// keeps the fields it does not name) and returns the previous value.
func (e *Env) setMaxRequestBodyMB(admin *Session, mb int) int {
	e.T.Helper()
	prev := int(admin.OK(e.T, http.MethodGet, "/settings/gateway", nil).Get("max_request_body_mb").Int())
	if got := admin.OK(e.T, http.MethodPut, "/settings/gateway", map[string]any{"max_request_body_mb": mb}).Get("max_request_body_mb").Int(); got != int64(mb) {
		e.T.Fatalf("PUT /settings/gateway max_request_body_mb=%d answered %d", mb, got)
	}
	return prev
}

// notServedMessage is the 404 of a request no account of the group serves
// on its endpoint (protocol binding, CONTRACTS 110).
const notServedMessage = "no account in this group serves this model on this endpoint"

// expectNotServed requires that 404, in the endpoint's error format
// ("openai", "anthropic" or "gemini").
func expectNotServed(t testing.TB, g *GatewayResult, format string) {
	t.Helper()
	j := g.JSON()
	msg := j.Get("error.message").String()
	ok := g.Status == http.StatusNotFound && msg == notServedMessage
	switch format {
	case "openai":
		ok = ok && j.Get("error.type").String() != "" && !j.Get("type").Exists()
	case "anthropic":
		ok = ok && j.Get("type").String() == "error"
	}
	if !ok {
		t.Fatalf("want 404 %q in %s format, got HTTP %d %s", notServedMessage, format, g.Status, g.Body)
	}
}

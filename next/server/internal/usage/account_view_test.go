package usage

import (
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/httpapi"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/testutil"
)

// scopedAuthz grants each user its keys.
type scopedAuthz struct {
	allowAll
	keys map[int64][]string
}

func (a scopedAuthz) PermissionSet(_ context.Context, uid int64) (core.PermissionSet, error) {
	return core.PermissionSetOf(a.keys[uid]...), nil
}

func (a scopedAuthz) Can(_ context.Context, uid int64, key string) (bool, error) {
	return core.PermissionSetOf(a.keys[uid]...).Has(key), nil
}

// The account side's fixture: an agent (team: a supplier and the supplier's
// own sub-supplier), the supplier, an operator, an outsider and a user
// without the permission; one account each (the supplier's second one
// deleted), and records on each, one with no account and one retried from
// the outsider's account onto the supplier's.
type accountSide struct {
	*fixture
	agent, supplier, sub, operator, outsider       int64
	accAgent, accSup, accSupDel, accSub, accOutsdr int64
	ids                                            map[string]int64 // usage_logs id by request id
}

func newAccountSide(t *testing.T) *accountSide {
	f := newFixture(t)
	ctx := t.Context()
	s := &accountSide{fixture: f, ids: map[string]int64{}}
	for name, u := range map[string]*int64{"agent": &s.agent, "supplier": &s.supplier, "sub": &s.sub, "operator": &s.operator, "outsider": &s.outsider} {
		*u = testutil.InsertUser(t, f.db, name+"@example.com")
	}
	testutil.SetOwner(t, f.db, s.supplier, s.agent)
	testutil.SetOwner(t, f.db, s.sub, s.supplier)
	account := func(name string, owner int64) int64 {
		var id int64
		require.NoError(t, f.db.SQL.QueryRowContext(ctx, `INSERT INTO accounts(name,plugin_key,type,credentials_enc,owner_id,created_by)
			VALUES($1,'anthropic','apikey','',$2,$2) RETURNING id`, name, owner).Scan(&id))
		return id
	}
	s.accAgent, s.accSup, s.accSupDel = account("Agent acc", s.agent), account("Sup acc", s.supplier), account("Sup old", s.supplier)
	s.accSub, s.accOutsdr = account("Sub acc", s.sub), account("Outsider acc", s.outsider)
	_, err := f.db.SQL.ExecContext(ctx, `UPDATE accounts SET deleted_at = now() WHERE id = $1`, s.accSupDel)
	require.NoError(t, err)

	now := time.Now()
	rec := func(id string, acc int64) *core.UsageRecord {
		r := f.record(id, false)
		r.CreatedAt = now.Add(-30 * time.Second)
		if acc != 0 {
			r.AccountID = &acc
		}
		return r
	}
	trail := rec("as-trail", s.accSup)
	trail.Attempts, trail.StatusCode = 2, 200
	trail.AttemptTrail = &core.AttemptTrail{V: 1, Steps: []core.AttemptStep{
		{AccountID: s.accOutsdr, Status: 502, ErrorType: "upstream_error", Ms: 640, Message: "upstream connect error", Next: core.AttemptSwitch},
		{AccountID: s.accSup, Status: 200, Ms: 900},
	}}
	sup2 := rec("as-sup-2", s.accSup)
	sup2.Model = "claude-haiku-4"
	f.svc.process(ctx, []*core.UsageRecord{rec("as-agent", s.accAgent), rec("as-sup", s.accSup), rec("as-sup-del", s.accSupDel),
		rec("as-sub", s.accSub), rec("as-outsider", s.accOutsdr), rec("as-none", 0), trail, sup2})
	rows, err := f.db.SQL.QueryContext(ctx, `SELECT request_id, id FROM usage_logs`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var rid string
		var id int64
		require.NoError(t, rows.Scan(&rid, &id))
		s.ids[rid] = id
	}
	require.NoError(t, rows.Err())

	az := scopedAuthz{
		keys: map[int64][]string{
			s.agent:    {"usage:account:read:team"},
			s.supplier: {"usage:account:read:own"},
			s.sub:      {"usage:account:read:own"},
			s.operator: {"usage:account:read"},
		},
	}
	f.h = gin.New()
	f.svc.RegisterRoutes(httpapi.NewRouter(f.h, allowAll{}, az))
	return s
}

func (s *accountSide) requestIDs(uid int64, path string) []string {
	var out []string
	for _, r := range s.get(uid, path, 200)["data"].([]any) {
		out = append(out, r.(map[string]any)["request_id"].(string))
	}
	return out
}

func keysOf(m any) []string {
	return slices.Sorted(maps.Keys(m.(map[string]any)))
}

func sortedStrings(v ...string) []string { return slices.Sorted(slices.Values(v)) }

// accountRecordKeys are the fields of the account side's records (CONTRACTS
// §102.5), exactly.
var accountRecordKeys = sortedStrings("id", "request_id", "created_at", "account_id", "account_name", "plugin_key", "platform",
	"account_type", "protocol", "endpoint", "model", "upstream_model", "stream", "status_code", "success", "error_type", "attempts",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cache_creation_1h_tokens", "standard_cost",
	"latency_ms", "first_token_ms", "attempt_trail")

var accountTotalsKeys = sortedStrings("requests", "success", "input_tokens", "output_tokens", "cache_read_tokens",
	"cache_creation_tokens", "prompt_tokens", "standard_cost", "latency_ms_avg", "first_token_ms_avg")

// Each caller reads the records of the accounts in its scope: the supplier
// its own (a deleted one included), the agent its team's, the operator
// every account's; a record without an account is nobody's, and a caller
// without the permission is refused.
func TestAccountUsageScopes(t *testing.T) {
	s := newAccountSide(t)
	sup := []string{"as-sup", "as-sup-del", "as-trail", "as-sup-2"}
	cases := []struct {
		name string
		uid  int64
		want []string
	}{
		{"supplier", s.supplier, sup},
		{"sub-supplier", s.sub, []string{"as-sub"}},
		{"agent", s.agent, append([]string{"as-agent", "as-sub"}, sup...)},
		{"operator", s.operator, append([]string{"as-agent", "as-sub", "as-outsider"}, sup...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ElementsMatch(t, tc.want, s.requestIDs(tc.uid, "/account-usage?page_size=50"))
			require.ElementsMatch(t, tc.want, s.requestIDs(tc.uid, "/account-usage?cursor="))
			total := s.get(tc.uid, "/account-usage", 200)["page"].(map[string]any)["total"]
			require.Equal(t, float64(len(tc.want)), total)
			for rid, id := range s.ids {
				want := 404
				if slices.Contains(tc.want, rid) {
					want = 200
				}
				s.get(tc.uid, "/account-usage/"+strconv.FormatInt(id, 10), want)
			}
		})
	}
	s.get(s.user, "/account-usage", 403)
	s.get(s.user, "/account-usage/overview", 403)

	// account_id narrows within the scope: another's account gives nothing.
	require.Empty(t, s.requestIDs(s.supplier, "/account-usage?account_id="+strconv.FormatInt(s.accOutsdr, 10)))
	require.ElementsMatch(t, []string{"as-sup", "as-trail", "as-sup-2"}, s.requestIDs(s.agent, "/account-usage?account_id="+strconv.FormatInt(s.accSup, 10)))
	require.ElementsMatch(t, []string{"as-sup-2"}, s.requestIDs(s.supplier, "/account-usage?model=claude-haiku-4"))
	require.ElementsMatch(t, []string{"as-sup"}, s.requestIDs(s.supplier, "/account-usage?request_id=as-sup&from=2026-01-01T00:00:00Z"))
}

// The records carry the allowlisted fields alone; the trail names the
// accounts of the scope and numbers the others, without messages.
func TestAccountUsageFields(t *testing.T) {
	s := newAccountSide(t)
	for _, r := range s.get(s.supplier, "/account-usage", 200)["data"].([]any) {
		require.Equal(t, accountRecordKeys, keysOf(r))
	}
	trailID := strconv.FormatInt(s.ids["as-trail"], 10)
	d := s.get(s.supplier, "/account-usage/"+trailID, 200)["data"].(map[string]any)
	require.Equal(t, sortedStrings(append(slices.Clone(accountRecordKeys), "metrics")...), keysOf(d))
	require.Equal(t, "Sup acc", d["account_name"])
	hidden := map[string]any{"account_ref": float64(1), "status": float64(502), "error_type": "upstream_error", "ms": float64(640), "next": "switch"}
	named := map[string]any{"account_id": float64(s.accSup), "account_name": "Sup acc", "status": float64(200), "ms": float64(900)}
	for _, uid := range []int64{s.supplier, s.agent} {
		list := s.get(uid, "/account-usage?request_id=as-trail", 200)["data"].([]any)
		require.Len(t, list, 1)
		for _, v := range []any{list[0].(map[string]any)["attempt_trail"], s.get(uid, "/account-usage/"+trailID, 200)["data"].(map[string]any)["attempt_trail"]} {
			steps := v.(map[string]any)["steps"].([]any)
			require.Equal(t, []any{hidden, named}, steps)
		}
	}
	steps := s.get(s.operator, "/account-usage/"+trailID, 200)["data"].(map[string]any)["attempt_trail"].(map[string]any)["steps"].([]any)
	require.Equal(t, map[string]any{"account_id": float64(s.accOutsdr), "account_name": "Outsider acc", "status": float64(502),
		"error_type": "upstream_error", "ms": float64(640), "next": "switch"}, steps[0])
}

// A parameter outside the allowlist is refused by name, on every route that
// filters, and so are the groupings and the ranking the side may not use.
func TestAccountUsageRefusedFilters(t *testing.T) {
	s := newAccountSide(t)
	refused := func(path, field string) {
		t.Helper()
		e := s.get(s.supplier, path, 400)["error"].(map[string]any)
		fields := e["details"].(map[string]any)["fields"].([]any)
		require.Equal(t, []any{map[string]any{"field": field, "code": "not_allowed", "message": field + " is not a filter of the account usage"}}, fields, path)
	}
	for _, q := range []string{"user_id=1", "api_key_id=1", "group_id=1", "user_email=a", "client_ip=192.0.2.1",
		"client_request_id=x", "billing_status=settled", "account_name=Sup", "api_key_name=k", "group_name=g", "node_id=n",
		"upstream_protocol=p", "upstream_model=m", "conversion=native", "model_contains=c"} {
		field, _, _ := strings.Cut(q, "=")
		for _, route := range []string{"/account-usage", "/account-usage/summary", "/account-usage/realtime", "/account-usage/export"} {
			refused(route+"?"+q, field)
		}
	}
	refused("/account-usage/overview?model=m", "model")
	refused("/account-usage/summary?page=2", "page")
	refused("/account-usage?lang=en", "lang")
	for _, g := range []string{"user", "api_key", "group", "ip", "upstream_model", "week"} {
		s.get(s.supplier, "/account-usage/summary?group_by="+g, 400)
	}
	s.get(s.supplier, "/account-usage/summary?order=cost", 400)
	s.get(s.supplier, "/account-usage/summary?limit=3", 200)
}

// The summary groups the scope's records by account with its name, without
// the charge; the overview counts the scope's accounts; answers are cached
// per scope, so two callers never read each other's.
func TestAccountUsageStats(t *testing.T) {
	s := newAccountSide(t)
	s.svc.overviews = newSnapshotCache[Overview](overviewFresh, statsCacheTTL)
	s.svc.summaries = newSnapshotCache[[]SummaryRow](statsCacheTTL, statsCacheTTL)
	byAccount := func(uid int64) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, r := range s.get(uid, "/account-usage/summary?group_by=account", 200)["data"].([]any) {
			m := r.(map[string]any)
			require.Equal(t, sortedStrings(append(slices.Clone(accountTotalsKeys), "key", "account_name", "total_tokens", "anomalies", "estimated")...), keysOf(m))
			out[m["key"].(string)] = m
		}
		return out
	}
	for range 2 { // the second round from the cache
		sup := byAccount(s.supplier)
		require.Len(t, sup, 2)
		require.Equal(t, "Sup acc", sup[strconv.FormatInt(s.accSup, 10)]["account_name"])
		require.Equal(t, float64(3), sup[strconv.FormatInt(s.accSup, 10)]["requests"])
		require.Equal(t, "Sup old", sup[strconv.FormatInt(s.accSupDel, 10)]["account_name"])
		require.Len(t, byAccount(s.agent), 4)
		require.Len(t, byAccount(s.operator), 5)
	}
	none := s.get(s.supplier, "/account-usage/summary?group_by=none", 200)["data"].([]any)
	require.Equal(t, sortedStrings(append(slices.Clone(accountTotalsKeys), "key", "total_tokens", "anomalies", "estimated")...), keysOf(none[0]))
	ranked := s.get(s.agent, "/account-usage/summary?group_by=account&order=requests&limit=1", 200)["data"].([]any)
	require.Len(t, ranked, 2)
	require.Equal(t, strconv.FormatInt(s.accSup, 10), ranked[0].(map[string]any)["key"])
	require.Equal(t, "__other__", ranked[1].(map[string]any)["key"])

	overview := func(uid int64) map[string]any {
		ov := s.get(uid, "/account-usage/overview?tz=UTC", 200)["data"].(map[string]any)
		require.Equal(t, sortedStrings("tz", "today", "yesterday", "last_30d", "last_5m", "accounts"), keysOf(ov))
		for _, w := range []string{"today", "yesterday", "last_30d"} {
			require.Equal(t, accountTotalsKeys, keysOf(ov[w]))
		}
		return ov
	}
	for range 2 {
		for uid, want := range map[int64][2]float64{s.supplier: {4, 1}, s.agent: {6, 3}, s.operator: {7, 4}, s.sub: {1, 1}} {
			ov := overview(uid)
			require.Equal(t, want[0], ov["last_5m"].(map[string]any)["requests"], uid)
			require.Equal(t, want[1], ov["accounts"].(map[string]any)["total"], uid)
		}
	}

	rt := s.get(s.supplier, "/account-usage/realtime", 200)["data"].(map[string]any)
	require.Equal(t, map[string]any{"rpm": float64(4), "tpm": float64(4 * 102000), "recent_failures": float64(0)}, rt)
	require.Equal(t, float64(6), s.get(s.agent, "/account-usage/realtime", 200)["data"].(map[string]any)["rpm"])
}

// The summary reads the rollup's hours over the scope too: skewing the
// rollup shows in the scope's account and in no other.
func TestAccountUsageRollup(t *testing.T) {
	s := newAccountSide(t)
	ctx := t.Context()
	old := func(id string, acc int64) *core.UsageRecord {
		r := s.record(id, false)
		r.CreatedAt, r.AccountID = time.Now().Add(-72*time.Hour), &acc
		return r
	}
	s.svc.process(ctx, []*core.UsageRecord{old("as-old-sup", s.accSup), old("as-old-out", s.accOutsdr)})
	for {
		hours, err := s.svc.refreshRollup(ctx)
		require.NoError(t, err)
		if hours == 0 {
			break
		}
	}
	path := "/account-usage/summary?group_by=account&from=" + time.Now().Add(-96*time.Hour).UTC().Format(time.RFC3339)
	requests := func() map[string]float64 {
		out := map[string]float64{}
		for _, r := range s.get(s.supplier, path, 200)["data"].([]any) {
			out[r.(map[string]any)["key"].(string)] = r.(map[string]any)["requests"].(float64)
		}
		return out
	}
	sup, del := strconv.FormatInt(s.accSup, 10), strconv.FormatInt(s.accSupDel, 10)
	require.Equal(t, map[string]float64{sup: 4, del: 1}, requests())
	_, err := s.db.SQL.ExecContext(ctx, `UPDATE usage_hourly SET requests = requests + 1000`)
	require.NoError(t, err)
	require.Equal(t, map[string]float64{sup: 1004, del: 1}, requests())
}

// The export has the allowlisted columns, the scope's rows and the call
// chain as the trail shows it, and is audited as the account side's.
func TestAccountUsageExport(t *testing.T) {
	s := newAccountSide(t)
	export := func(uid int64, query string) []string {
		req := httptest.NewRequest("GET", "/api/v1/account-usage/export?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+strconv.FormatInt(uid, 10))
		w := httptest.NewRecorder()
		s.h.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code, w.Body.String())
		return strings.Split(strings.TrimSpace(strings.TrimPrefix(w.Body.String(), "\uFEFF")), "\n")
	}
	lines := export(s.supplier, "lang=en")
	require.Equal(t, strings.Join(append(slices.Clone(accountExportColumns), "Call chain"), ","), lines[0])
	require.Len(t, lines, 1+4)
	lines = export(s.supplier, "lang=en&request_id=as-trail")
	cells := strings.Split(lines[1], ",")
	require.Equal(t, "Account 1 → #"+strconv.FormatInt(s.accSup, 10), cells[len(cells)-1])
	require.Len(t, export(s.agent, "lang=zh"), 1+6)
	var detail json.RawMessage
	require.NoError(t, s.db.SQL.QueryRowContext(t.Context(), `SELECT detail FROM audit_logs WHERE action = 'usage.export' AND user_id = $1
		ORDER BY id DESC LIMIT 1`, s.supplier).Scan(&detail))
	require.JSONEq(t, `{"rows":1,"truncated":false,"query":"lang=en&request_id=as-trail","view":"account"}`, string(detail))
}

// Every column of the account side's export is one of the export's.
func TestAccountExportColumnsExist(t *testing.T) {
	t.Parallel()
	for _, name := range accountExportColumns {
		require.True(t, slices.ContainsFunc(exportColumns, func(c exportColumn) bool { return c.en == name }), name)
	}
}

package standard_test

import (
	"cmp"
	"context"
	"reflect"
	"regexp"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/anthropic"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/gemini"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/providers/openai"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/standard"
)

// relayManifest declares one relay-site type whose upstream address is the
// account's base_url (the host rule of every relay and balance request).
const relayManifest = `{
  "apiVersion": 1, "key": "relaysite", "version": "0.1.0", "publisher": "test", "runtime": "grpc",
  "capabilities": [{"id": "platform.adapter.v1"}],
  "accountTypes": [{"id": "apikey", "upstreamFields": ["base_url"], "balance": {},
    "platforms": [{"platform": "openai"}, {"platform": "anthropic"}, {"platform": "gemini"}]}]
}`

// newAPISite is the New API shape (design §3.5.2): two requests, the key's
// quota with the key, the site's unit of account without it.
func newAPISite() standard.Type {
	t := standard.Merge("apikey", relaySpec, openai.APIKey(""), anthropic.APIKey(""), gemini.APIKey(""))
	t.Balance = &standard.Balance{
		Requests: []standard.BalanceRequest{{Path: "/api/usage/token/"}, {Path: "/api/status", Auth: &standard.Auth{}}},
		Read:     readNewAPI,
	}
	return t
}

func readNewAPI(_ *standard.Request, answers []*pluginv1.BalanceAnswer) *pluginv1.BalanceResult {
	for _, a := range answers {
		if f := standard.BalanceFailure(a); f != nil {
			return f
		}
	}
	token, site := gjson.ParseBytes(answers[0].GetBody()), gjson.ParseBytes(answers[1].GetBody())
	perUnit := site.Get("data.quota_per_unit").Float()
	switch {
	case !token.Get("code").Bool():
		return transientResult("token usage: " + token.Get("message").String())
	case perUnit <= 0:
		return transientResult("no quota_per_unit")
	}
	d, user := token.Get("data"), token.Get("data.user_quota.total_available")
	var available float64
	switch {
	case !d.Get("unlimited_quota").Bool():
		available = d.Get("total_available").Float()
		if user.Exists() {
			available = min(available, user.Float())
		}
	case user.Exists():
		available = user.Float()
	default:
		return &pluginv1.BalanceResult{Unlimited: true}
	}
	return &pluginv1.BalanceResult{AmountMicros: standard.USDMicros(available, perUnit), Currency: "USD"}
}

// sub2APISite is the Sub2API shape: one request, the amount under one of
// three names, -1 for no limit, the unit in the answer.
func sub2APISite() standard.Type {
	t := standard.Merge("apikey", relaySpec, openai.APIKey(""), anthropic.APIKey(""), gemini.APIKey(""))
	t.Balance = &standard.Balance{Requests: []standard.BalanceRequest{{Path: "/v1/usage"}}, Read: readSub2API}
	return t
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func readSub2API(_ *standard.Request, answers []*pluginv1.BalanceAnswer) *pluginv1.BalanceResult {
	if f := standard.BalanceFailure(answers[0]); f != nil {
		return f
	}
	b := gjson.ParseBytes(answers[0].GetBody())
	if v := b.Get("isValid"); v.Exists() && !v.Bool() {
		return &pluginv1.BalanceResult{ErrorType: pluginv1.BalanceResult_ERROR_TYPE_AUTH_REJECTED, ErrorMessage: "key disabled or expired"}
	}
	var amount gjson.Result
	for _, path := range []string{"remaining", "balance", "quota.remaining"} {
		if amount = b.Get(path); amount.Exists() {
			break
		}
	}
	unit := cmp.Or(b.Get("unit").String(), "USD")
	switch {
	case !amount.Exists():
		return transientResult("no amount")
	case amount.Float() == -1:
		return &pluginv1.BalanceResult{Unlimited: true}
	case !currencyCode.MatchString(unit):
		return transientResult("unit " + unit)
	}
	return &pluginv1.BalanceResult{AmountMicros: standard.USDMicros(amount.Float(), 1), Currency: unit}
}

func transientResult(msg string) *pluginv1.BalanceResult {
	return &pluginv1.BalanceResult{ErrorType: pluginv1.BalanceResult_ERROR_TYPE_TRANSIENT, ErrorMessage: msg}
}

// answers serves the balance requests from bodies by URL, recording what
// was asked.
func answers(bodies map[string]string, seen *[]*pluginv1.BalanceRequest) func(*pluginv1.BalanceRequest) *pluginv1.BalanceAnswer {
	return func(r *pluginv1.BalanceRequest) *pluginv1.BalanceAnswer {
		*seen = append(*seen, r)
		body, ok := bodies[r.GetUrl()]
		if !ok {
			return &pluginv1.BalanceAnswer{Status: 404}
		}
		return &pluginv1.BalanceAnswer{Status: 200, Body: []byte(body)}
	}
}

func TestBalanceNewAPI(t *testing.T) {
	h := pluginsdktest.Start(t, standard.New(newAPISite()), pluginsdktest.Options{Manifest: []byte(relayManifest)})
	const tokenURL, statusURL = "https://relay.example.com/api/usage/token/", "https://relay.example.com/api/status"
	site := `{"success":true,"data":{"quota_per_unit":500000}}`
	for name, c := range map[string]struct {
		token string
		want  *pluginv1.BalanceResult
	}{
		"limited":               {`{"code":true,"data":{"total_available":1250000,"unlimited_quota":false}}`, &pluginv1.BalanceResult{AmountMicros: 2500000, Currency: "USD"}},
		"the lower of two":      {`{"code":true,"data":{"total_available":1250000,"unlimited_quota":false,"user_quota":{"total_available":250000}}}`, &pluginv1.BalanceResult{AmountMicros: 500000, Currency: "USD"}},
		"unlimited, user quota": {`{"code":true,"data":{"unlimited_quota":true,"user_quota":{"total_available":50000}}}`, &pluginv1.BalanceResult{AmountMicros: 100000, Currency: "USD"}},
		"unlimited":             {`{"code":true,"data":{"unlimited_quota":true}}`, &pluginv1.BalanceResult{Unlimited: true}},
		"code false":            {`{"code":false,"message":"no such token"}`, transientResult("token usage: no such token")},
	} {
		var seen []*pluginv1.BalanceRequest
		got, err := h.Balance(context.Background(), relayAccount, answers(map[string]string{tokenURL: c.token, statusURL: site}, &seen))
		if err != nil || got.String() != c.want.String() {
			t.Errorf("%s: %v %v, want %v", name, got, err, c.want)
		}
		// Both requests, in order, GET; the key only on the first.
		if len(seen) != 2 || seen[0].Url != tokenURL || seen[1].Url != statusURL || seen[0].Method != "GET" ||
			!reflect.DeepEqual(seen[0].Headers, map[string]string{"authorization": "Bearer site-key-0123"}) || len(seen[1].Headers) != 0 {
			t.Fatalf("%s: requests %v", name, seen)
		}
	}
	var seen []*pluginv1.BalanceRequest
	got, err := h.Balance(context.Background(), relayAccount, answers(map[string]string{tokenURL: `{"code":true,"data":{}}`}, &seen))
	if err != nil || got.GetErrorType() != pluginv1.BalanceResult_ERROR_TYPE_TRANSIENT || got.GetErrorMessage() != "upstream returned HTTP 404" {
		t.Errorf("status failed: %v %v", got, err)
	}
}

func TestBalanceSub2API(t *testing.T) {
	h := pluginsdktest.Start(t, standard.New(sub2APISite()), pluginsdktest.Options{Manifest: []byte(relayManifest)})
	const usageURL = "https://relay.example.com/v1/usage"
	for body, want := range map[string]*pluginv1.BalanceResult{
		`{"isValid":true,"remaining":12.5,"unit":"USD"}`: {AmountMicros: 12500000, Currency: "USD"},
		`{"balance":3}`: {AmountMicros: 3000000, Currency: "USD"},
		`{"quota":{"remaining":7.25},"unit":"CNY"}`: {AmountMicros: 7250000, Currency: "CNY"},
		`{"remaining":-1}`:                          {Unlimited: true},
		`{"isValid":false,"remaining":1}`:           {ErrorType: pluginv1.BalanceResult_ERROR_TYPE_AUTH_REJECTED, ErrorMessage: "key disabled or expired"},
		`{"remaining":1,"unit":"dollars"}`:          transientResult("unit dollars"),
	} {
		var seen []*pluginv1.BalanceRequest
		got, err := h.Balance(context.Background(), relayAccount, answers(map[string]string{usageURL: body}, &seen))
		if err != nil || got.String() != want.String() || len(seen) != 1 || seen[0].Headers["authorization"] != "Bearer site-key-0123" {
			t.Errorf("%s: %v %v %v, want %v", body, got, err, seen, want)
		}
	}
	got, err := h.Balance(context.Background(), relayAccount, func(*pluginv1.BalanceRequest) *pluginv1.BalanceAnswer {
		return &pluginv1.BalanceAnswer{Status: 401, Body: []byte(`{"error":"invalid key"}`)}
	})
	if err != nil || got.GetErrorType() != pluginv1.BalanceResult_ERROR_TYPE_AUTH_REJECTED || got.GetErrorMessage() != `upstream returned HTTP 401: {"error":"invalid key"}` {
		t.Errorf("401: %v %v", got, err)
	}
}

// The fake host refuses a request to a host the manifest does not allow
// for the account, before any is answered, and a type without Balance is
// UNIMPLEMENTED.
func TestBalanceHostRules(t *testing.T) {
	noAddress := `{"apiVersion": 1, "key": "relaysite", "version": "0.1.0", "publisher": "test", "runtime": "grpc",
	  "capabilities": [{"id": "platform.adapter.v1"}], "accountTypes": [{"id": "apikey", "platforms": [{"platform": "openai"}]}]}`
	h := pluginsdktest.Start(t, standard.New(newAPISite()), pluginsdktest.Options{Manifest: []byte(noAddress)})
	called := false
	if _, err := h.Balance(context.Background(), relayAccount, func(*pluginv1.BalanceRequest) *pluginv1.BalanceAnswer { called = true; return nil }); status.Code(err) != codes.PermissionDenied || called {
		t.Errorf("a request off the host rules: %v, answered %v", err, called)
	}
	h = pluginsdktest.Start(t, standard.New(relaySite()), pluginsdktest.Options{Manifest: []byte(relayManifest)})
	if _, err := h.Balance(context.Background(), relayAccount, nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("no balance: %v", err)
	}
	if _, err := h.Platform.ParseBalanceResponse(context.Background(), &pluginv1.ParseBalanceResponseRequest{Account: relayAccount}); status.Code(err) != codes.Unimplemented {
		t.Errorf("no balance, parse: %v", err)
	}
}

// Read sees one answer per request; another count is a transient result.
func TestBalanceAnswerCount(t *testing.T) {
	p := standard.New(newAPISite())
	out, err := p.ParseBalanceResponse(context.Background(), &pluginv1.ParseBalanceResponseRequest{Account: relayAccount, Answers: []*pluginv1.BalanceAnswer{{Status: 200}}})
	if err != nil || out.GetResult().GetErrorType() != pluginv1.BalanceResult_ERROR_TYPE_TRANSIENT {
		t.Errorf("one answer to two requests: %v %v", out, err)
	}
}

func TestBalanceFailure(t *testing.T) {
	for _, c := range []struct {
		answer *pluginv1.BalanceAnswer
		want   *pluginv1.BalanceResult
	}{
		{&pluginv1.BalanceAnswer{Status: 200}, nil},
		{&pluginv1.BalanceAnswer{Status: 204}, nil},
		{&pluginv1.BalanceAnswer{TransportError: "dial tcp: refused"}, transientResult("transport error: dial tcp: refused")},
		{&pluginv1.BalanceAnswer{Status: 403}, &pluginv1.BalanceResult{ErrorType: pluginv1.BalanceResult_ERROR_TYPE_AUTH_REJECTED, ErrorMessage: "upstream returned HTTP 403"}},
		{&pluginv1.BalanceAnswer{Status: 429, Body: []byte(" slow down \n")}, transientResult("upstream returned HTTP 429: slow down")},
		{&pluginv1.BalanceAnswer{Status: 503}, transientResult("upstream returned HTTP 503")},
		{&pluginv1.BalanceAnswer{Status: 302}, transientResult("upstream returned HTTP 302")},
	} {
		if got := standard.BalanceFailure(c.answer); got.String() != c.want.String() {
			t.Errorf("%v: %v, want %v", c.answer, got, c.want)
		}
	}
	for _, c := range []struct {
		units, perUSD float64
		want          int64
	}{{1250000, 500000, 2500000}, {1, 3, 333333}, {2, 3, 666667}, {12.5, 1, 12500000}, {0, 500000, 0}} {
		if got := standard.USDMicros(c.units, c.perUSD); got != c.want {
			t.Errorf("USDMicros(%v, %v) = %d, want %d", c.units, c.perUSD, got, c.want)
		}
	}
}

func TestBalanceCheck(t *testing.T) {
	for name, b := range map[string]*standard.Balance{
		"no request":        {Read: readSub2API},
		"no read":           {Requests: []standard.BalanceRequest{{Path: "/v1/usage"}}},
		"a relative path":   {Requests: []standard.BalanceRequest{{Path: "v1/usage"}}, Read: readSub2API},
		"a bad auth header": {Requests: []standard.BalanceRequest{{Path: "/v1/usage", Auth: &standard.Auth{Header: "Authorization"}}}, Read: readSub2API},
	} {
		ty := sub2APISite()
		ty.Balance = b
		if err := standard.Check(ty); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

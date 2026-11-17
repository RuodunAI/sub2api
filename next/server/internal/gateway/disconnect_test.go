package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// hangUpAfterStart sends a streaming request whose upstream holds after
// message_start, reads that first event and then drops the connection.
func hangUpAfterStart(t *testing.T, e *env) {
	t.Helper()
	hangUpAfter(t, e, "message_start")
}

// hangUpAfter sends a streaming request, reads the stream up to the data of
// event and then drops the connection.
func hangUpAfter(t *testing.T, e *env, event string) {
	t.Helper()
	raw, _ := json.Marshal(body(testModel, true))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+"/v1/messages", strings.NewReader(string(raw)))
	req.Header.Set("x-api-key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for seen := false; ; {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before %s: %v", event, err)
		}
		if seen && strings.HasPrefix(l, "data: ") {
			break
		}
		seen = seen || strings.TrimSpace(l) == "event: "+event
	}
	cancel()
	_ = resp.Body.Close()
}

// reportedPrompt is the prompt side of the mock upstream's message_start.
var reportedPrompt = core.UsageTokens{Input: upInput, CacheRead: upCacheRead,
	CacheCreation: upCacheCreation - upCacheCreate1h, CacheCreation1h: upCacheCreate1h}

func promptSide(t core.UsageTokens) core.UsageTokens {
	t.Output = 0
	return t
}

// holdingEnv is an env whose upstream holds after message_start until the
// returned channel is closed, under the given disconnect policy.
func holdingEnv(t *testing.T, policy string, opts ...envOpt) (*env, chan struct{}) {
	t.Helper()
	e := newEnv(t, opts...)
	e.balance.disconnect = policy
	hold := make(chan struct{})
	e.up.set("acc-1", &upstreamRule{hold: hold})
	return e, hold
}

// awaitUpstreamCanceled fails unless the upstream request is canceled soon.
func awaitUpstreamCanceled(t *testing.T, e *env, why string) {
	t.Helper()
	select {
	case <-e.up.canceled:
	case <-time.After(10 * time.Second):
		t.Fatalf("upstream request was not canceled %s", why)
	}
}

// wantPrechargeWithReportedPrompt checks the record of a hang-up billed the
// estimate: the prompt side is the one the answer reported, never the
// estimate's on top of it.
func wantPrechargeWithReportedPrompt(t *testing.T, rec *core.UsageRecord) {
	t.Helper()
	if rec.ErrorType != errTypeClientCanceled || !rec.Billable || rec.UsageExtract != core.UsageExtractPrecharge {
		t.Fatalf("record: billable = %v type = %q extract = %q", rec.Billable, rec.ErrorType, rec.UsageExtract)
	}
	if promptSide(rec.Tokens) != reportedPrompt {
		t.Fatalf("prompt side %+v, want the reported %+v", promptSide(rec.Tokens), reportedPrompt)
	}
}

// awaitUpstreamKept fails if the upstream request, still holding, is
// canceled although its answer is to be read on.
func awaitUpstreamKept(t *testing.T, e *env) {
	t.Helper()
	select {
	case key := <-e.up.canceled:
		t.Fatalf("upstream request of %s was canceled although the answer is to be read on", key)
	case <-time.After(300 * time.Millisecond):
	}
}

// wantReadOnBilledReported checks the record of a hang-up whose answer was
// read to its end: still a hang-up, billed what the upstream reported.
func wantReadOnBilledReported(t *testing.T, rec *core.UsageRecord) {
	t.Helper()
	want := reportedPrompt
	want.Output = upOutput
	if rec.Tokens != want {
		t.Fatalf("tokens %+v, want %+v", rec.Tokens, want)
	}
	if rec.Success || rec.ErrorType != errTypeClientCanceled || rec.StatusCode != statusClientClosed {
		t.Fatalf("record does not say the client left: %+v", rec)
	}
	if !rec.Billable || rec.UsageExtract == core.UsageExtractPrecharge {
		t.Fatalf("billable = %v extract = %q, want the reported usage billed", rec.Billable, rec.UsageExtract)
	}
}

// Under "continue" the upstream answer is read to its end after the client
// left and the request pays what the upstream reported (CONTRACTS §25.8).
func TestClientDisconnectContinueBillsReportedUsage(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectContinue)

	hangUpAfterStart(t, e)
	// The upstream must outlive the client: it is still holding.
	awaitUpstreamKept(t, e)
	close(hold)
	wantReadOnBilledReported(t, e.record())
}

// A "continue" read has no wait of its own: the request's timeouts end it,
// the same as with the client still there, and the request falls back to
// the pre-charge estimate.
func TestClientDisconnectContinueEndsWithRelayTimeout(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectContinue,
		withEnvironment(core.EnvironmentValues{core.EnvVarRelayTimeoutSeconds: 1}, nil))
	defer close(hold)

	hangUpAfterStart(t, e)
	awaitUpstreamCanceled(t, e, "at the relay timeout")
	wantPrechargeWithReportedPrompt(t, e.record())
}

// Shutdown ends an answer that is only still read for its usage: a client
// that left is no reason to hold the server open.
func TestClientDisconnectContinueStopsAtShutdown(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectContinue)
	defer close(hold)

	hangUpAfterStart(t, e)
	select {
	case key := <-e.up.canceled:
		t.Fatalf("upstream request of %s was canceled before shutdown", key)
	case <-time.After(300 * time.Millisecond):
	}
	e.gw.AbandonReads()
	awaitUpstreamCanceled(t, e, "at shutdown")
	wantPrechargeWithReportedPrompt(t, e.record())
}

// "precharge" stops the upstream at once and bills the estimate.
func TestClientDisconnectPrechargeBillsPromptOnce(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectPrecharge)
	defer close(hold)

	hangUpAfterStart(t, e)
	awaitUpstreamCanceled(t, e, "when the client left")
	wantPrechargeWithReportedPrompt(t, e.record())
}

// "free" stops the upstream at once and bills nothing.
func TestClientDisconnectFreeBillsNothing(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectFree)
	defer close(hold)

	hangUpAfterStart(t, e)
	awaitUpstreamCanceled(t, e, "when the client left")
	if rec := e.record(); rec.ErrorType != errTypeClientCanceled || rec.Billable {
		t.Fatalf("record: billable = %v type = %q", rec.Billable, rec.ErrorType)
	}
}

// testOutputReserve is the output_reserve_tokens of the tests below: what a
// hang-up before the final usage pays for as output.
const testOutputReserve = 4000

// wantServed checks the record of a request whose client left only after the
// final usage reached it: a success, billed what the upstream reported.
func wantServed(t *testing.T, rec *core.UsageRecord) {
	t.Helper()
	want := reportedPrompt
	want.Output = upOutput
	if !rec.Success || rec.StatusCode != http.StatusOK || rec.ErrorType != "" {
		t.Fatalf("not recorded as served: success = %v status = %d type = %q", rec.Success, rec.StatusCode, rec.ErrorType)
	}
	if rec.Tokens != want || !rec.Billable || rec.UsageExtract == core.UsageExtractPrecharge {
		t.Fatalf("tokens = %+v billable = %v extract = %q, want the reported %+v", rec.Tokens, rec.Billable, rec.UsageExtract, want)
	}
}

// wantHangUpBilledReserve checks the record of a request whose client left
// before the final usage: a hang-up, its output raised to the reserve.
func wantHangUpBilledReserve(t *testing.T, rec *core.UsageRecord) {
	t.Helper()
	if rec.Success || rec.StatusCode != statusClientClosed {
		t.Fatalf("not recorded as a hang-up: success = %v status = %d", rec.Success, rec.StatusCode)
	}
	wantPrechargeWithReportedPrompt(t, rec)
	if rec.Tokens.Output != testOutputReserve {
		t.Fatalf("output %d, want the reserve %d", rec.Tokens.Output, testOutputReserve)
	}
}

// A client that hangs up once the final usage event (message_delta) reached
// it, while the upstream still holds the stream open - Codex closes on
// response.completed - was served, under every policy: a success billed what
// the upstream reported, and the upstream is not read on.
func TestClientDisconnectAfterFinalUsageIsServed(t *testing.T) {
	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue, core.DisconnectFree} {
		for _, last := range []string{"message_delta", "message_stop"} {
			t.Run(policy+"/"+last, func(t *testing.T) {
				e := newEnv(t)
				e.balance.disconnect = policy
				e.balance.outputReserve = testOutputReserve
				e.up.set("acc-1", &upstreamRule{linger: true})

				hangUpAfter(t, e, last)
				awaitUpstreamCanceled(t, e, "when the served client left")
				wantServed(t, e.record())
			})
		}
	}
}

// A hang-up before the final usage event stays one: 499 and the output
// raised to the reserve.
func TestClientDisconnectBeforeFinalUsageBillsReserve(t *testing.T) {
	e, hold := holdingEnv(t, core.DisconnectPrecharge)
	defer close(hold)
	e.balance.outputReserve = testOutputReserve

	hangUpAfterStart(t, e)
	awaitUpstreamCanceled(t, e, "when the client left")
	wantHangUpBilledReserve(t, e.record())
}

// A request the plugin executes (platform.execute.v1) through the host's
// forward follows the same rules, in the record the execution commits: the
// policy applies to a hang-up before the final usage ("continue" reads the
// answer on), and one after it was served under every policy.
func TestClientDisconnectExecutedRequest(t *testing.T) {
	setup := func(t *testing.T, policy string, rule *upstreamRule) (*env, chan *core.UsageRecord) {
		svc := &commitCapture{
			statsExecutionStore: statsExecutionStore{fakeSettler: &fakeSettler{ch: make(chan *core.UsageRecord, 8)}},
			commits:             make(chan *core.UsageRecord, 8),
		}
		e := newEnv(t, func(e *env) {
			e.gen.accountTypes[0].Client = &executeTestPlatform{PlatformPlugin: e.plat}
			e.man.Capabilities = append(e.man.Capabilities, manifest.Capability{ID: manifest.CapPlatformExecute})
		})
		e.gw.d.Settler = svc
		e.balance.disconnect = policy
		e.balance.outputReserve = testOutputReserve
		e.up.set("acc-1", rule)
		return e, svc.commits
	}
	committed := func(t *testing.T, commits chan *core.UsageRecord) *core.UsageRecord {
		t.Helper()
		select {
		case rec := <-commits:
			return rec
		case <-time.After(5 * time.Second):
			t.Fatal("no committed usage")
			return nil
		}
	}

	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue} {
		t.Run("after final usage/"+policy, func(t *testing.T) {
			e, commits := setup(t, policy, &upstreamRule{linger: true})
			hangUpAfter(t, e, "message_stop")
			awaitUpstreamCanceled(t, e, "when the served client left")
			wantServed(t, committed(t, commits))
		})
	}
	t.Run("before final usage/precharge", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		e, commits := setup(t, core.DisconnectPrecharge, &upstreamRule{hold: hold})
		hangUpAfterStart(t, e)
		awaitUpstreamCanceled(t, e, "when the client left")
		wantHangUpBilledReserve(t, committed(t, commits))
	})
	t.Run("before final usage/continue", func(t *testing.T) {
		hold := make(chan struct{})
		e, commits := setup(t, core.DisconnectContinue, &upstreamRule{hold: hold})
		hangUpAfterStart(t, e)
		awaitUpstreamKept(t, e)
		close(hold)
		wantReadOnBilledReported(t, committed(t, commits))
	})
}

func TestCanceledTokens(t *testing.T) {
	est := core.UsageTokens{Input: 1000, Output: 4000}
	for name, tc := range map[string]struct{ reported, want core.UsageTokens }{
		"nothing reported":      {core.UsageTokens{}, est},
		"only output reported":  {core.UsageTokens{Output: 9}, est},
		"output above reserve":  {core.UsageTokens{Output: 5000}, core.UsageTokens{Input: 1000, Output: 5000}},
		"prompt fully cached":   {core.UsageTokens{CacheRead: 990}, core.UsageTokens{CacheRead: 990, Output: 4000}},
		"prompt above estimate": {core.UsageTokens{Input: 1500, Output: 2}, core.UsageTokens{Input: 1500, Output: 4000}},
		"1h write": {core.UsageTokens{Input: 3, CacheCreation1h: 997},
			core.UsageTokens{Input: 3, CacheCreation1h: 997, Output: 4000}},
	} {
		if got := canceledTokens(tc.reported, est); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Sub2API-Devs/sup2api/next/sdk/manifest"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/gateway/convert"
)

// hangUpAfterStart sends a streaming request whose upstream holds after
// message_start, reads that first event and then drops the connection.
func hangUpAfterStart(t *testing.T, e *env) {
	t.Helper()
	hangUpAfter(t, e, "message_start")
}

// hangUpAfter sends a streaming /v1/messages request, reads the stream up to
// the data of event and then drops the connection.
func hangUpAfter(t *testing.T, e *env, event string) {
	t.Helper()
	_, hangUp := openStream(t, e, "/v1/messages", body(testModel, true), nil, `"type":"`+event+`"`)
	hangUp()
}

// openStream sends a streaming request to path, with headers over the test
// key's x-api-key (an empty value removes one), and reads the stream up to a
// data line holding marker. The caller reads on from the reader, or drops
// the connection with hangUp.
func openStream(t *testing.T, e *env, path string, b any, headers map[string]string, marker string) (_ *bufio.Reader, hangUp func()) {
	t.Helper()
	raw, _ := json.Marshal(b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+path, strings.NewReader(string(raw)))
	req.Header.Set("x-api-key", testKey)
	for k, v := range headers {
		req.Header.Del(k)
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	hangUp = func() {
		cancel()
		_ = resp.Body.Close()
	}
	br := bufio.NewReader(resp.Body)
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			hangUp()
			t.Fatalf("stream ended before %s: %v", marker, err)
		}
		if strings.HasPrefix(l, "data:") && strings.Contains(l, marker) {
			return br, hangUp
		}
	}
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
// the upstream reported, counted as no cancellation, and the upstream is not
// read on.
func TestClientDisconnectAfterFinalUsageIsServed(t *testing.T) {
	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue, core.DisconnectFree} {
		for _, last := range []string{"message_delta", "message_stop"} {
			t.Run(policy+"/"+last, func(t *testing.T) {
				e := newEnv(t)
				e.balance.disconnect = policy
				e.balance.outputReserve = testOutputReserve
				e.up.set("acc-1", &upstreamRule{linger: true})
				stats := &statsCapture{ch: make(chan accountStatsSample, 8)}
				e.gw.d.Observations = stats

				hangUpAfter(t, e, last)
				awaitUpstreamCanceled(t, e, "when the served client left")
				wantServed(t, e.record())
				if v := stats.next(t); !v.Success || v.Canceled {
					t.Fatalf("account statistics: success = %v canceled = %v", v.Success, v.Canceled)
				}
			})
		}
	}
}

// On a converting route the final usage is the upstream's: a chat client of
// an Anthropic upstream that hangs up on [DONE] was served.
func TestClientDisconnectAfterFinalUsageIsServedConverted(t *testing.T) {
	e := newEnv(t, func(e *env) { e.conv = convert.Default() })
	e.balance.outputReserve = testOutputReserve
	e.up.set("acc-1", &upstreamRule{linger: true})
	path, input := openAIConversionInput("openai.chat", true)

	_, hangUp := openStream(t, e, path, input, map[string]string{"authorization": "Bearer " + testKey, "x-api-key": ""}, "[DONE]")
	hangUp()
	awaitUpstreamCanceled(t, e, "when the served client left")
	rec := e.record()
	if rec.Protocol != "openai.chat" || rec.UpstreamProtocol != "anthropic.messages" {
		t.Fatalf("not converted: %s from %s", rec.Protocol, rec.UpstreamProtocol)
	}
	wantServed(t, rec)
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
	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue} {
		t.Run("after final usage/"+policy, func(t *testing.T) {
			e, _, commits := executedEnv(t, policy, &upstreamRule{linger: true})
			hangUpAfter(t, e, "message_stop")
			awaitUpstreamCanceled(t, e, "when the served client left")
			wantServed(t, committed(t, commits))
		})
	}
	t.Run("before final usage/precharge", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		e, _, commits := executedEnv(t, core.DisconnectPrecharge, &upstreamRule{hold: hold})
		hangUpAfterStart(t, e)
		awaitUpstreamCanceled(t, e, "when the client left")
		wantHangUpBilledReserve(t, committed(t, commits))
	})
	t.Run("before final usage/continue", func(t *testing.T) {
		hold := make(chan struct{})
		e, _, commits := executedEnv(t, core.DisconnectContinue, &upstreamRule{hold: hold})
		hangUpAfterStart(t, e)
		awaitUpstreamKept(t, e)
		close(hold)
		wantReadOnBilledReported(t, committed(t, commits))
	})
}

// executedEnv is an env whose requests the plugin executes
// (platform.execute.v1) through the host's forward, under the given
// disconnect policy; commits are the usage records the executions commit.
func executedEnv(t *testing.T, policy string, rule *upstreamRule) (_ *env, _ *executeTestPlatform, commits chan *core.UsageRecord) {
	t.Helper()
	var p *executeTestPlatform
	e := newEnv(t, func(e *env) {
		p = &executeTestPlatform{PlatformPlugin: e.plat}
		e.gen.accountTypes[0].Client = p
		e.man.Capabilities = append(e.man.Capabilities, manifest.Capability{ID: manifest.CapPlatformExecute})
	})
	e.up.set("acc-1", rule)
	return e, p, captureCommits(e, policy)
}

// captureCommits makes e capture the usage the executions commit, under the
// given disconnect policy, and returns where it lands.
func captureCommits(e *env, policy string) chan *core.UsageRecord {
	svc := &commitCapture{statsExecutionStore: statsExecutionStore{fakeSettler: e.settler}, commits: make(chan *core.UsageRecord, 8)}
	e.gw.d.Settler = svc
	e.balance.disconnect = policy
	e.balance.outputReserve = testOutputReserve
	return svc.commits
}

// committed waits for the next usage record an execution commits.
func committed(t *testing.T, commits chan *core.UsageRecord) *core.UsageRecord {
	t.Helper()
	select {
	case rec := <-commits:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("no committed usage")
		return nil
	}
}

// A plugin that gives up its forward (killed, drained) while the client is
// still there is no hang-up, under any policy: the upstream is canceled at
// once, the client gets the truncated answer and the record says the
// upstream was interrupted.
func TestClientDisconnectExecutionAbortedIsNoHangUp(t *testing.T) {
	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue} {
		t.Run(policy, func(t *testing.T) {
			hold := make(chan struct{})
			defer close(hold)
			e, p, commits := executedEnv(t, policy, &upstreamRule{hold: hold})
			p.abort = make(chan struct{})

			br, hangUp := openStream(t, e, "/v1/messages", body(testModel, true), nil, `"type":"message_start"`)
			defer hangUp()
			close(p.abort)
			awaitUpstreamCanceled(t, e, "when the plugin gave up")
			if rest, _ := io.ReadAll(br); strings.Contains(string(rest), "message_stop") {
				t.Fatalf("the answer was not cut: %s", rest)
			}
			rec := committed(t, commits)
			if rec.Success || rec.ErrorType != errTypeUpstream || rec.StatusCode == statusClientClosed ||
				!strings.Contains(rec.ErrorMessage, errExecutionAborted.Error()) || rec.UsageExtract == core.UsageExtractPrecharge {
				t.Fatalf("record: success = %v type = %q status = %d message = %q extract = %q",
					rec.Success, rec.ErrorType, rec.StatusCode, rec.ErrorMessage, rec.UsageExtract)
			}
		})
	}
}

// The Codex case: an executed Responses request whose client hangs up on
// response.completed while the upstream keeps the stream open was served.
func TestClientDisconnectAfterResponsesCompletedIsServed(t *testing.T) {
	for _, policy := range []string{core.DisconnectPrecharge, core.DisconnectContinue} {
		t.Run(policy, func(t *testing.T) {
			e, plat, up := streamOnlyEnv(t, manifest.AccountPlatform{Platform: manifest.PlatformOpenAI, Endpoints: []string{"responses"}})
			b := &e.gen.accountTypes[len(e.gen.accountTypes)-1]
			b.Client = &executeTestPlatform{PlatformPlugin: plat}
			b.Plugin.Manifest.Capabilities = append(b.Plugin.Manifest.Capabilities, manifest.Capability{ID: manifest.CapPlatformExecute})
			commits := captureCommits(e, policy)
			up.linger = make(chan struct{}, 1)

			_, hangUp := openStream(t, e, "/v1/responses", map[string]any{"model": testModel, "input": "hi", "stream": true},
				map[string]string{"authorization": "Bearer " + testKey, "x-api-key": ""}, `"type":"response.completed"`)
			hangUp()
			select {
			case <-up.linger:
			case <-time.After(10 * time.Second):
				t.Fatal("upstream request was not canceled when the served client left")
			}
			rec := committed(t, commits)
			if !rec.Success || rec.StatusCode != http.StatusOK || rec.ErrorType != "" || !rec.Billable ||
				rec.Tokens.Output != soOutput || rec.Tokens.CacheRead != soCached || rec.UsageExtract == core.UsageExtractPrecharge {
				t.Fatalf("not recorded as served: %+v", rec)
			}
		})
	}
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

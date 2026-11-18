package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/protocol"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// The relay that waits (design §4.3): an endpoint whose usage the plugin
// reads, or a task submission. The request is sent inside the plugin's
// Execute call (ExecutionClient.Relay, on the runtime's goroutine while the
// handler's waits in Execute); the plugin gets the bounded observation and
// states the facts in Execute's return value. The execution's records make
// those facts durable before anything is published: BeginExecution before
// the send, ObserveExecution with the core's reading once the exchange
// ended, then CommitExecution with the facts or CloseExecutionUnreported
// without them (a free record, the precharge released, nothing billed).
// When the plugin gives up its call meanwhile the upstream is cut.

// commitTimeout bounds the records' writes of one execution, a commit with
// its retries included.
const commitTimeout = 15 * time.Second

// commitRetryDelay spaces the retries of a failed commit.
const commitRetryDelay = 250 * time.Millisecond

// waitedRelay is the exchange of one relay that waits.
type waitedRelay struct {
	x       *execution
	ctx     context.Context // the plugin's host call; its end cuts the upstream
	id      string
	records core.ExecutionRecords
	// result is what send decided; the attempt's when there is nothing to
	// settle (nothing observed, no upstream error).
	result attemptResult
	// failure is the upstream's error answer handed to the plugin, with
	// the parts kept for the client.
	failure   *pluginv1.ClassifyErrorRequest
	header    http.Header
	raw       []byte
	errorBody *upstreamErrorBody
	// The 2xx answer: the observation the plugin reads, whether its usage
	// may be read from it, the core's reading stored by ObserveExecution
	// (observed) or that could not be stored (unconfirmed), the answer held
	// back until the facts commit, and a task submission's answer.
	observation *pluginv1.ExtractUsageRequest
	extractable bool
	observed    bool
	unconfirmed bool
	base        *core.UsageRecord
	spool       *bufferedResponse
	taskBody    []byte
}

// relay is ExecutionClient.Relay: send req now and hand the plugin exactly
// one of the observation and the upstream's error, or an error when the
// exchange ended without either.
func (x *execution) relay(ctx context.Context, req *pluginv1.UpstreamRequest) (*pluginv1.RelayUpstreamResponse, error) {
	c := x.c
	w := &waitedRelay{x: x, ctx: ctx, id: uuid.NewString()}
	x.waited = w
	records, ok := c.g.d.Settler.(core.ExecutionRecords)
	if !ok {
		w.result = attemptResult{kind: attemptReturn, err: fromCore(core.ErrUnavailable.WithMessage("execution records unavailable"), errTypeInternal)}
		return nil, errors.New(w.result.err.Message)
	}
	w.records = records
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	err := records.BeginExecution(bctx, core.ExecutionIntent{TaskSubmit: c.ep.TaskSubmit(), Record: c.rec, ID: w.id, RequestID: c.rid,
		PluginKey: x.rt.binding.Plugin.Key, AccountID: x.acc.ID, Attempt: c.rec.Attempts})
	cancel()
	if err != nil {
		w.result = attemptResult{kind: attemptReturn, err: fromCore(core.ErrUnavailable.WithMessage("execution could not be recorded").WithCause(err), errTypeInternal)}
		return nil, err
	}
	w.result = c.send(x.network, x, req)
	switch {
	case w.failure != nil:
		return &pluginv1.RelayUpstreamResponse{Error: w.failure}, nil
	case w.observed:
		return &pluginv1.RelayUpstreamResponse{Observation: w.observation}, nil
	case w.result.err != nil:
		return nil, errors.New(w.result.err.Message)
	}
	return nil, errors.New("the upstream exchange ended without an answer")
}

// fail keeps an upstream failure for the plugin to classify; the attempt's
// result waits for its classification (settle).
func (w *waitedRelay) fail(status int, header http.Header, raw []byte, transportErr string) attemptResult {
	x := w.x
	prefix := raw
	if len(prefix) > classifyPrefix {
		prefix = prefix[:classifyPrefix]
	}
	w.header, w.raw = header, raw
	w.failure = &pluginv1.ClassifyErrorRequest{Meta: x.c.metaFor(x.rt), Account: x.acct, Status: int32(status),
		Headers: firstHeaderValues(header), BodyPrefix: prefix, TransportError: transportErr}
	return attemptResult{kind: attemptReturn}
}

// answer relays a 2xx answer for the plugin to read: a task submission's is
// read whole and kept, another is relayed to the client (an answer that is
// not an event stream into the spool, published once the facts commit).
// Then the core's reading is stored.
func (w *waitedRelay) answer(ctx context.Context, resp *http.Response) attemptResult {
	x := w.x
	c := x.c
	if c.ep.TaskSubmit() {
		c.rec.StatusCode = resp.StatusCode
		raw, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxTaskSnapshotBytes+1))
		if err != nil || len(raw) > protocol.MaxTaskSnapshotBytes {
			return c.taskFailure(ctx, "task response incomplete", err)
		}
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		err = c.g.d.Tasks.ReceiveTask(bctx, c.rid, resp.StatusCode, raw, "")
		cancel()
		if err != nil {
			return c.taskFailure(ctx, "task response receipt failed", err)
		}
		w.taskBody = raw
		c.armUsageExtraction(x.rt, x.acct, resp, &usageCapture{body: raw}, x.body)
		c.rec.Success = true
	} else {
		original := c.c.Writer
		if !isSSE(resp.Header.Get("Content-Type")) {
			w.spool = newBufferedResponse(original)
			c.c.Writer = w.spool
		}
		func() {
			defer func() { c.c.Writer = original }()
			w.result = c.forward(ctx, x.rt, x.acct, resp, x.body)
		}()
	}
	if p := c.usage; p != nil {
		w.observation = &pluginv1.ExtractUsageRequest{Meta: p.meta, Account: p.account, Status: int32(p.status), Headers: p.headers,
			Body: p.cap.body, Events: p.cap.events, Truncated: p.cap.stopped, Fields: p.fields, FieldsOmitted: p.fieldsOmitted}
		w.extractable = x.rt.pluginUsage && !p.cap.tooLarge && !c.ep.TaskSubmit()
	} else {
		w.observation = &pluginv1.ExtractUsageRequest{Meta: c.metaFor(x.rt), Account: x.acct, Status: int32(resp.StatusCode)}
	}
	c.usage = nil
	c.rec.LatencyMs = int(c.g.now().Sub(c.start) / time.Millisecond)
	c.stampSticky()
	w.base = cloneUsage(c.rec)
	if err := w.observe(ctx); err != nil {
		w.unconfirmed = true
		return attemptResult{kind: attemptReturn, err: fromCore(core.ErrUnavailable.WithMessage("usage recording could not be confirmed").WithCause(err), errTypeInternal)}
	}
	return attemptResult{kind: attemptDone}
}

// observe stores the core's reading of the answer: the record the
// execution is closed with when no facts come.
func (w *waitedRelay) observe(ctx context.Context) error {
	c := w.x.c
	reading := cloneUsage(w.base)
	if w.x.rt.pluginUsage {
		reading.UsageExtract = core.UsageExtractFallback
	}
	c.finalizeBillability(ctx, reading, c.ep.Billing)
	o := w.observation
	summary, _ := json.Marshal(map[string]any{"status": c.rec.StatusCode, "body_bytes": len(o.GetBody()), "event_count": len(o.GetEvents()),
		"truncated": o.GetTruncated(), "task_submission": c.ep.TaskSubmit()})
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if err := w.records.ObserveExecution(bctx, w.id, reading, summary); err != nil {
		return err
	}
	w.observed = true
	return nil
}

// settle ends the attempt once Execute returned: the upstream's error with
// the plugin's classification, the facts of an observed answer, or what
// send decided when neither came.
func (w *waitedRelay) settle(ctx context.Context, res core.ExecutionResult, err error) attemptResult {
	switch {
	case w.failure != nil:
		return w.classified(ctx, res.Response.GetClassification())
	case w.observed:
		if err != nil {
			return w.unreported(ctx, "execute failed: "+err.Error(), err)
		}
		return w.commit(ctx, res.Response)
	case w.unconfirmed:
		return w.unreported(ctx, "observation not stored", errors.New(w.result.err.Message))
	}
	return w.result
}

// classified applies the plugin's classification of the upstream's error,
// the default one when it gave none. A task submission is never retried.
func (w *waitedRelay) classified(ctx context.Context, cls *pluginv1.ClassifyErrorResponse) attemptResult {
	x, f := w.x, w.failure
	if cls == nil {
		cls = defaultClassification(int(f.GetStatus()))
	}
	res := x.c.applyClassification(ctx, x.rt, x.acct, int(f.GetStatus()), w.header, w.raw, f.GetTransportError(), cls)
	if x.c.ep.TaskSubmit() {
		res.kind = attemptReturn
	}
	x.c.preserveHTTPError(res.err, int(f.GetStatus()), w.header, w.errorBody)
	return res
}

// commit validates and prices the facts Execute returned and commits them,
// retrying the same commit within commitTimeout (it is idempotent); then the
// held-back answer is published.
func (w *waitedRelay) commit(ctx context.Context, out *pluginv1.ExecuteResponse) attemptResult {
	c := w.x.c
	previous := c.rec
	commit, body, err := w.facts(ctx, out)
	if err == nil {
		err = w.commitWithRetry(ctx, commit)
	}
	if err != nil {
		c.rec = previous
		return w.unreported(ctx, err.Error(), err)
	}
	c.taskPersisted = true
	if body != nil && w.spool == nil {
		w.spool = newBufferedResponse(c.c.Writer)
		w.spool.Header().Set("Content-Type", "application/json")
		w.spool.WriteHeader(c.rec.StatusCode)
		_, _ = w.spool.Write(body)
	}
	if w.spool == nil {
		return attemptResult{kind: attemptDone}
	}
	if w.spool.err != nil || !c.rec.Success {
		// The usage is committed although the buffered answer cannot be
		// delivered; dispatch counts no tokens for an attempt that is not done.
		c.countTokens(context.WithoutCancel(ctx), w.x.acc.ID)
		e := fromCore(core.ErrUnavailable.WithMessage("upstream response incomplete"), errTypeUpstream)
		e.LocalFailure = w.spool.err != nil // the spool failed here, not the account
		return attemptResult{kind: attemptReturn, err: e}
	}
	if err := w.spool.publish(); err != nil {
		slog.WarnContext(ctx, "gateway: committed response delivery failed", "request_id", c.rid, "err", err)
	}
	return attemptResult{kind: attemptDone}
}

// facts turns the report Execute returned into the commit, leaving the
// priced record in c.rec, and the answer to publish for a task (its
// upstream id replaced by the public one). A report of the wrong kind, or
// none, is no fact.
func (w *waitedRelay) facts(ctx context.Context, out *pluginv1.ExecuteResponse) (core.ExecutionCommit, []byte, error) {
	c, rt := w.x.c, w.x.rt
	c.rec = cloneUsage(w.base)
	commit := core.ExecutionCommit{ID: w.id, Operation: core.ExecutionOpUsage, Record: c.rec}
	var body []byte
	switch r := out.GetReport().(type) {
	case *pluginv1.ExecuteResponse_Usage:
		if c.ep.TaskSubmit() {
			return commit, nil, errors.New("usage reported for a task submission")
		}
		if w.extractable {
			c.applyUsageReport(ctx, rt, rt.binding.Plugin.Key, r.Usage)
		} else {
			c.usageFallback(ctx, rt, "the observation was too large to read the usage from", nil)
		}
		commit.Digest = core.FactDigest(r.Usage)
	case *pluginv1.ExecuteResponse_HostUsage:
		if c.ep.TaskSubmit() {
			return commit, nil, errors.New("host_usage reported for a task submission")
		}
		if rt.pluginUsage {
			c.usageFallback(ctx, rt, "the plugin took the host's reading", nil)
		}
		commit.Digest = core.FactDigest(nil)
	case *pluginv1.ExecuteResponse_Task:
		task, returned, err := w.task(ctx, r.Task)
		if err != nil {
			return commit, nil, err
		}
		commit.Operation, commit.Digest, commit.Task, body = core.ExecutionOpWatch, core.FactDigest(r.Task), task, returned
	default:
		return commit, nil, errors.New("no report")
	}
	c.finalizeBillability(ctx, c.rec, c.ep.Billing)
	commit.Record = c.rec
	if commit.Task != nil {
		commit.Task.Record = c.rec
	}
	return commit, body, nil
}

// task checks a task submission's facts against the answer and registers
// it under the request's public id.
func (w *waitedRelay) task(ctx context.Context, parsed *pluginv1.TaskSubmission) (*core.TaskRegistration, []byte, error) {
	c := w.x.c
	if !c.ep.TaskSubmit() {
		return nil, nil, errors.New("task reported for an endpoint that submits none")
	}
	ref := parsed.GetUpstreamRefId()
	if !refIDRe.MatchString(ref) {
		return nil, nil, errors.New("invalid task identity")
	}
	returned, err := protocol.RewriteTaskID(w.taskBody, c.ep.Task.IDPaths, ref, c.taskPublicID)
	if err != nil {
		return nil, nil, err
	}
	query, ok := c.taskPair(true)
	if !ok {
		return nil, nil, errors.New("missing task query contract")
	}
	snapshot := []byte(parsed.GetSnapshotJson())
	if _, err = protocol.RewriteTaskID(snapshot, query.Task.IDPaths, ref, c.taskPublicID); err != nil {
		return nil, nil, err
	}
	if rv := parsed.GetUsage().GetReserve(); rv != nil && rv.GetRefId() != ref {
		return nil, nil, errors.New("reservation identity mismatch")
	}
	if parsed.GetUsage() != nil {
		c.applyUsageReport(ctx, w.x.rt, c.plugin.Key, parsed.GetUsage())
	}
	return &core.TaskRegistration{PublicID: c.taskPublicID, UpstreamID: ref, Kind: c.ep.Task.Kind, Snapshot: snapshot, IDPaths: query.Task.IDPaths,
		NextCheckAfter: time.Duration(parsed.GetNextCheckAfterSec()) * time.Second, Deadline: time.Duration(parsed.GetDeadlineSec()) * time.Second}, returned, nil
}

// commitWithRetry commits, retrying a failure within commitTimeout; a
// conflict (other facts, or the execution closed meanwhile) is final.
func (w *waitedRelay) commitWithRetry(ctx context.Context, commit core.ExecutionCommit) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	for {
		err := w.records.CommitExecution(cctx, commit)
		var ce *core.Error
		if err == nil || errors.As(err, &ce) && ce.Code == core.ErrConflict.Code {
			return err
		}
		select {
		case <-cctx.Done():
			return fmt.Errorf("commit failed: %w", err)
		case <-time.After(commitRetryDelay):
		}
	}
}

// unreported ends an execution whose answer reached the upstream but whose
// facts did not come (or could not be committed): it is closed without
// them - a free record, the precharge released, nothing billed - and its
// records own the request's usage. A task is not registered; an answer
// held back is not published (503), one already streamed ends abruptly.
func (w *waitedRelay) unreported(ctx context.Context, reason string, cause error) attemptResult {
	c := w.x.c
	if w.observed {
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		if err := w.records.CloseExecutionUnreported(bctx, core.ExecutionUnreported{ID: w.id, Reason: reason}); err != nil {
			slog.ErrorContext(ctx, "gateway: close unreported execution", "request_id", c.rid, "execution", w.id, "err", err)
		}
		cancel()
	}
	c.usage = nil
	c.taskPersisted = true
	if c.ep.TaskSubmit() {
		return c.taskFailure(ctx, "task execution could not be confirmed", cause)
	}
	if c.c.Writer.Written() {
		panic(http.ErrAbortHandler)
	}
	return attemptResult{kind: attemptReturn, err: fromCore(core.ErrUnavailable.WithMessage("usage recording could not be confirmed").WithCause(cause), errTypeInternal)}
}

// close frees the held-back answer.
func (w *waitedRelay) close() {
	if w.spool != nil {
		w.spool.close()
	}
}

// cloneUsage is a deep copy of r, the host's cache evidence included.
func cloneUsage(r *core.UsageRecord) *core.UsageRecord {
	raw, _ := json.Marshal(r)
	var out core.UsageRecord
	_ = json.Unmarshal(raw, &out)
	out.Metrics = preserveHostCacheEvidence(r.Metrics, out.Metrics)
	for i := range out.Additional {
		out.Additional[i].Metrics = preserveHostCacheEvidence(r.Additional[i].Metrics, out.Additional[i].Metrics)
	}
	for i := range out.Replacement {
		out.Replacement[i].Metrics = preserveHostCacheEvidence(r.Replacement[i].Metrics, out.Replacement[i].Metrics)
	}
	return &out
}

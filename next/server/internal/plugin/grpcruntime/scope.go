package grpcruntime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/core"
)

// errSegmentTimeout cancels an Execute call whose plugin spent longer than
// the platform call timeout on one stretch of its own work.
var errSegmentTimeout = errors.New("plugin exceeded the platform call timeout")

// transferBytesPerSecond is the rate at which a segment's bound assumes a
// message crosses between the core and the plugin process: serialised,
// written to the unix socket, read and decoded on the other side. Measured
// on a loaded host, one direction took 0.2-0.3 s for 16 MiB (about
// 55-80 MiB/s), 1.0-1.1 s for 128 MiB and 3.7-5.0 s for 512 MiB (about
// 100-120 MiB/s); with the host at a load of 65 on 8 cores, a 64 MiB body
// there and back took 3.6 s (about 36 MiB/s). 8 MiB/s is a quarter of the
// slowest of these: the segment timeout is there to catch a hung plugin,
// not to police throughput, so the allowance errs on the generous side (a
// hung plugin with a 128 MiB body is caught after 2 s + 32 s).
const transferBytesPerSecond = 8 << 20

// transferAllowance is the time n bytes may take to cross the process
// boundary at transferBytesPerSecond.
func transferAllowance(n int64) time.Duration {
	return time.Duration(n) * time.Second / transferBytesPerSecond
}

// callScopes are the host-client authorities of the Execute and Monitor
// calls in flight on one process, by execution token.
type callScopes struct {
	mu      sync.Mutex
	byToken map[string]*callScope
}

// callScope is the authority of one Execute or Monitor call: what its
// execution token lets the plugin's RelayUpstream and ExecuteHTTP calls do.
// It lives exactly as long as the call.
type callScope struct {
	ctx    context.Context // the call's; ends when it returns
	client core.ExecutionClient
	// account is the account of the call: a fetch may go to the hosts of its
	// own upstream addresses.
	account *pluginv1.Account
	// relay: an Execute call (a Monitor call only fetches); wait: its
	// request allows the waiting form.
	relay, wait bool
	// bodyBytes is the size of the request body the call handed to the
	// plugin: a relayed body may be that large whatever the limit says.
	bodyBytes int64
	// segment bounds each stretch of the plugin's own work (nil: the call
	// has one overall timeout). It runs whenever no host call is active,
	// for segmentLimit.
	segment        *time.Timer
	segmentTimeout time.Duration

	mu           sync.Mutex
	closed, busy bool
	done         chan struct{} // closed when the active host call ends
	// The rest belongs to the one active host call (busy) and is read by
	// the call's owner after close joined it.
	relayed bool
	parked  *pluginv1.UpstreamRequest // a relay that did not wait
	fetches int
	// evidence: a fetch of this call completed with an HTTP status, without
	// a transport error and untruncated. A terminal Monitor result needs it.
	evidence bool
}

// open registers scope under a fresh token.
func (s *callScopes) open(scope *callScope) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byToken == nil {
		s.byToken = map[string]*callScope{}
	}
	s.byToken[token] = scope
	return token, nil
}

// close voids the token and waits for the host call in progress, whose
// context cancel has ended: once close returns, nothing of the client runs.
// The caller cancels the call's context first.
func (s *callScopes) close(token string, scope *callScope) {
	s.mu.Lock()
	delete(s.byToken, token)
	s.mu.Unlock()
	scope.mu.Lock()
	scope.closed = true
	if scope.segment != nil {
		scope.segment.Stop()
	}
	done := scope.done
	scope.mu.Unlock()
	if done != nil {
		<-done
	}
}

// enter starts one host call of the execution token names. The plugin's
// calls of one execution are serial; leave ends the call.
func (h *hostServer) enter(token string) (*callScope, error) {
	if h.p == nil || len(token) != 64 {
		return nil, status.Error(codes.PermissionDenied, "invalid execution token")
	}
	h.p.scopes.mu.Lock()
	sc := h.p.scopes.byToken[token]
	h.p.scopes.mu.Unlock()
	if sc == nil {
		return nil, status.Error(codes.PermissionDenied, "unknown or expired execution token")
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed || sc.ctx.Err() != nil {
		return nil, status.Error(codes.PermissionDenied, "expired execution token")
	}
	if sc.busy {
		return nil, status.Error(codes.FailedPrecondition, "another host call of this execution is active")
	}
	sc.busy = true
	sc.done = make(chan struct{})
	if sc.segment != nil {
		sc.segment.Stop()
	}
	return sc, nil
}

// leave ends the host call enter started, whose answer of answerBytes is
// still to reach the plugin; the plugin's next stretch of work begins.
func (sc *callScope) leave(answerBytes int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.busy = false
	if sc.segment != nil && !sc.closed {
		sc.segment.Reset(sc.segmentLimit(int64(answerBytes)))
	}
	close(sc.done)
}

// segmentLimit bounds the plugin's next stretch of work: the platform call
// timeout plus the time to move what the stretch carries across the
// process boundary, which is no work of the plugin's. A stretch carries
// what the host hands the plugin as it begins (sent: the Execute request,
// the answer of a host call) and, until the plugin has relayed, the request
// body it is to hand back in its relay: the host cannot know the size of a
// host call before it has received it, so it counts on the body it handed
// out. Small bodies keep the bare timeout.
func (sc *callScope) segmentLimit(sent int64) time.Duration {
	if sc.relay && !sc.relayed {
		sent += sc.bodyBytes
	}
	return sc.segmentTimeout + transferAllowance(sent)
}

// hostContext is the context of the client's work for one host call: it
// ends with the Execute/Monitor call and when the plugin gives up the host
// call (rpc), whichever is first.
func (sc *callScope) hostContext(rpc context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(sc.ctx)
	stop := context.AfterFunc(rpc, cancel)
	return ctx, func() { stop(); cancel() }
}

// shallowClone copies the fields of in without copying what they refer to:
// the runtime adds the execution token to a request without changing the
// caller's message or copying its body.
func shallowClone[M proto.Message](in M) M {
	out := in.ProtoReflect().New()
	in.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		out.Set(fd, v)
		return true
	})
	return out.Interface().(M)
}

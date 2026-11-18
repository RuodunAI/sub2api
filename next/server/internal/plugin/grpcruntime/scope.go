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
	// has one overall timeout). It runs whenever no host call is active.
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

// leave ends the host call enter started; the plugin's next stretch of work
// begins.
func (sc *callScope) leave() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.busy = false
	if sc.segment != nil && !sc.closed {
		sc.segment.Reset(sc.segmentTimeout)
	}
	close(sc.done)
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

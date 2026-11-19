package codex

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/Sub2API-Devs/sup2api/next/sdk/gen/pluginv1"
	"github.com/Sub2API-Devs/sup2api/next/sdk/pluginsdk/pluginsdktest"
)

// testSalt is a session salt the tests store before a request.
const testSalt = "test-salt"

// seedSalt stores salt as the cluster's session salt in the harness KV.
func seedSalt(t *testing.T, h *pluginsdktest.Harness, salt string) {
	t.Helper()
	if _, err := h.Host.KVSet(context.Background(), &pluginv1.KVSetRequest{Namespace: kvSessionSalt, Key: sessionSaltKey, Value: []byte(salt)}); err != nil {
		t.Fatal(err)
	}
}

// storedSalt is the session salt in the harness KV, "" for none.
func storedSalt(t *testing.T, h *pluginsdktest.Harness) string {
	t.Helper()
	r, err := h.Host.KVGet(context.Background(), &pluginv1.KVGetRequest{Namespace: kvSessionSalt, Key: sessionSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	return string(r.GetValue())
}

// sessionOf relays a request of API key 7 with prompt_cache_key conv-1 and
// returns its session header and the whole relayed request.
func sessionOf(t *testing.T, h *pluginsdktest.Harness) (string, *pluginv1.UpstreamRequest) {
	t.Helper()
	r := relayed(t, h, execRequest(TypeDevice, ProtocolResponses, `{"model":"gpt-5.4","prompt_cache_key":"conv-1"}`))
	return r.GetHeaders()["session_id"], r
}

// The session salt is created once, kept in the KV and used by every later
// request; it never leaves the plugin.
func TestSessionSaltCreated(t *testing.T) {
	h := startExecuting(t, DefaultEndpoints)
	if storedSalt(t, h) != "" {
		t.Fatal("salt before any request")
	}
	id, r := sessionOf(t, h)
	salt := storedSalt(t, h)
	if len(salt) < 16 || id != upstreamSessionID([]byte(salt), 7, "conv-1") {
		t.Fatalf("salt %q, session %q", salt, id)
	}
	if again, _ := sessionOf(t, h); again != id || storedSalt(t, h) != salt {
		t.Fatalf("second request: session %q salt %q, want %q %q", again, storedSalt(t, h), id, salt)
	}
	for k, v := range r.GetHeaders() {
		if strings.Contains(k, salt) || strings.Contains(v, salt) {
			t.Fatalf("header %s carries the salt", k)
		}
	}
	if bytes.Contains(r.GetBody(), []byte(salt)) || strings.Contains(r.GetUrl(), salt) {
		t.Fatal("the request carries the salt")
	}
	// Without a usable prompt_cache_key no salt is needed.
	h = startExecuting(t, DefaultEndpoints)
	relayed(t, h, execRequest(TypeDevice, ProtocolResponses, `{"model":"gpt-5.4"}`))
	if storedSalt(t, h) != "" {
		t.Fatal("salt created for a request without a session")
	}
}

// The session depends on the salt: two processes (nodes, or one before and
// after a restart) with the same stored salt agree, another salt gives
// another session, and the unsalted hash of before is not what is sent.
func TestSessionSaltKeysTheSession(t *testing.T) {
	a, b, other := startExecuting(t, DefaultEndpoints), startExecuting(t, DefaultEndpoints), startExecuting(t, DefaultEndpoints)
	seedSalt(t, a, testSalt)
	seedSalt(t, b, testSalt)
	seedSalt(t, other, "another-salt")
	idA, _ := sessionOf(t, a)
	idB, _ := sessionOf(t, b)
	idOther, _ := sessionOf(t, other)
	if idA != idB || idA != upstreamSessionID([]byte(testSalt), 7, "conv-1") {
		t.Fatalf("same salt: %q vs %q", idA, idB)
	}
	if idOther == idA || idOther != upstreamSessionID([]byte("another-salt"), 7, "conv-1") {
		t.Fatalf("other salt: %q vs %q", idOther, idA)
	}
	if idA == upstreamSessionID(nil, 7, "conv-1") {
		t.Fatal("the salt does not change the session")
	}
}

// While another node holds the salt lock and has not stored the salt yet,
// the request fails as unavailable and nothing is relayed or stored.
func TestSessionSaltBeingCreated(t *testing.T) {
	h := startExecuting(t, DefaultEndpoints)
	if r, err := h.Host.LockAcquire(context.Background(), &pluginv1.LockAcquireRequest{Name: sessionSaltLock, TtlMs: 60000, Token: "another-node-token-0001"}); err != nil || !r.GetAcquired() {
		t.Fatalf("lock = %v %v", r, err)
	}
	ex := h.Execute(context.Background(), execRequest(TypeDevice, ProtocolResponses, `{"model":"gpt-5.4","prompt_cache_key":"conv-1"}`))
	if status.Code(ex.Err) != codes.Unavailable || len(ex.Relays()) != 0 || storedSalt(t, h) != "" {
		t.Fatalf("err %v, relays %d, salt %q", ex.Err, len(ex.Relays()), storedSalt(t, h))
	}
}

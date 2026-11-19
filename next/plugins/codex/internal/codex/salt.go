package codex

import (
	"context"
	"crypto/rand"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The session salt keys the hash of the upstream session (upstreamSession),
// so the upstream cannot recover API key ids from the sessions it receives.
// It is never sent anywhere. One salt serves every node and survives
// restarts: it lives in the plugin KV without expiry, written once by the
// first request that needs it, under a cluster lock so two nodes never write
// different ones. Every request reads it, so all nodes follow the stored one
// (should the KV lose it, the next request writes a new one and every
// conversation gets a new upstream session once).

const (
	// kvSessionSalt and sessionSaltKey are the KV namespace and key of the
	// salt.
	kvSessionSalt  = "session_salt"
	sessionSaltKey = "v1"
	// sessionSaltLock guards writing the salt; sessionSaltLockTTL covers one
	// read and one write of the KV (the lock is renewed while held).
	sessionSaltLock    = "codex-session-salt"
	sessionSaltLockTTL = 10 * time.Second
)

// sessionSalt returns the cluster's session salt, creating it the first
// time. While another node holds the lock and has not written it yet, the
// call fails with Unavailable.
func (p *Plugin) sessionSalt(ctx context.Context) ([]byte, error) {
	kv, err := p.kv()
	if err != nil {
		return nil, err
	}
	if salt, found, err := kv.Get(ctx, kvSessionSalt, sessionSaltKey); err != nil || found {
		return salt, err
	}
	_, err = p.host.Locks().WithLock(ctx, sessionSaltLock, sessionSaltLockTTL, func(ctx context.Context) error {
		if _, found, err := kv.Get(ctx, kvSessionSalt, sessionSaltKey); err != nil || found {
			return err
		}
		return kv.Set(ctx, kvSessionSalt, sessionSaltKey, []byte(rand.Text()), 0)
	})
	if err != nil {
		return nil, err
	}
	salt, found, err := kv.Get(ctx, kvSessionSalt, sessionSaltKey)
	if err == nil && !found {
		err = status.Error(codes.Unavailable, "the upstream session salt is being created, retry / 上游会话盐正在生成，请重试")
	}
	return salt, err
}

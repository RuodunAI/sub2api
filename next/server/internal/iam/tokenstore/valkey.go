package tokenstore

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Keys, all under the s2a: prefix the core's Valkey data shares:
//
//	s2a:sess:{hash}         HASH of one session (uid, sid, method, ip, ua,
//	                        created_at, last_seen, tv; prev after a rotation),
//	                        TTL the idle timeout capped at the absolute one
//	s2a:sid:{sid}           STRING: the hash of the session's current token,
//	                        same TTL
//	s2a:usess:{uid}         ZSET of the user's session ids by last use: the
//	                        sessions list and the bulk revocations
//	s2a:flow:{purpose}:{hash}  a flow, TTL FlowTTL, taken with GETDEL
//	s2a:flow:window:{name}  a step-up window: when it was opened
const keyPrefix = "s2a:"

func sessKey(hash string) string       { return keyPrefix + "sess:" + hash }
func sidKey(id string) string          { return keyPrefix + "sid:" + id }
func userKey(uid int64) string         { return keyPrefix + "usess:" + strconv.FormatInt(uid, 10) }
func flowKey(purpose, h string) string { return keyPrefix + "flow:" + purpose + ":" + h }
func windowKey(name string) string     { return keyPrefix + "flow:window:" + name }

// Defaults of Options.
const (
	DefaultIdle        = 7 * 24 * time.Hour
	DefaultMax         = 30 * 24 * time.Hour
	DefaultTouchEvery  = 5 * time.Minute
	DefaultRotateGrace = 30 * time.Second
	DefaultFlowTTL     = 5 * time.Minute
	// DefaultMaxSessions bounds the sessions of a user: a login past it ends
	// the one used least recently.
	DefaultMaxSessions = 50
)

// authTimeout bounds each Valkey call that authenticates a request (Lookup,
// Touch, Drop): a stuck server fails the console request with 503 within
// it instead of holding it.
const authTimeout = time.Second

// Options tune Valkey; zero fields take the defaults.
type Options struct {
	Idle        time.Duration // a session ends unused this long
	Max         time.Duration // and this long after it started, used or not
	TouchEvery  time.Duration // a use is written only this long after the last written one
	RotateGrace time.Duration // a rotated token keeps working this long
	FlowTTL     time.Duration
	MaxSessions int              // sessions a user keeps at most
	Now         func() time.Time // tests
}

// Valkey implements SessionStore and FlowStore.
type Valkey struct {
	rdb redis.Cmdable
	o   Options
}

var (
	_ SessionStore = (*Valkey)(nil)
	_ FlowStore    = (*Valkey)(nil)
)

// NewValkey returns the stores on rdb (NewClient makes the one to use).
func NewValkey(rdb redis.Cmdable, o Options) *Valkey {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Idle, DefaultIdle)
	def(&o.Max, DefaultMax)
	def(&o.TouchEvery, DefaultTouchEvery)
	def(&o.RotateGrace, DefaultRotateGrace)
	def(&o.FlowTTL, DefaultFlowTTL)
	if o.MaxSessions <= 0 {
		o.MaxSessions = DefaultMaxSessions
	}
	if o.Idle > o.Max {
		o.Idle = o.Max
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Valkey{rdb: rdb, o: o}
}

// NewClient is the client of the stores, on the server base names: it
// retries once instead of three times, waits a second at most for a
// connection (dialled or from the pool) or an answer, and honours the
// deadline of each call (authTimeout), so an outage costs a console request
// a 503 quickly instead of a long hang. A call is retried after its answer
// was lost, so the session scripts are idempotent; a flow taken (GETDEL)
// and retried that way finds nothing and reads as expired, and the user
// starts that step again. The caller closes it.
func NewClient(base *redis.Options) *redis.Client {
	o := *base
	o.Dialer = nil // base's dialer has base's timeout; this one is built from o
	o.MaxRetries = 1
	o.DialTimeout, o.ReadTimeout, o.WriteTimeout, o.PoolTimeout = time.Second, time.Second, time.Second, time.Second
	o.ContextTimeoutEnabled = true
	return redis.NewClient(&o)
}

// Idle is the idle timeout of a session.
func (v *Valkey) Idle() time.Duration { return v.o.Idle }

// FlowTTL is how long a flow lives.
func (v *Valkey) FlowTTL() time.Duration { return v.o.FlowTTL }

// fail maps a Valkey error: redis.Nil is a missing key. ctx is the caller's
// (not a deadline of the call's own): its end is the caller leaving, not an
// outage.
func fail(ctx context.Context, op string, err error) error {
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if ctx.Err() != nil {
		return ErrUnavailable.WithCause(err) // the caller left, not an outage
	}
	return unavailable(ctx, op, err)
}

// ------------------------------------------------------------ sessions

// ttl is how long s may still live at now: the idle timeout, capped at the
// absolute one.
func (v *Valkey) ttl(s Session, now time.Time) time.Duration {
	return min(v.o.Idle, s.CreatedAt.Add(v.o.Max).Sub(now))
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// fields are the HASH fields of s.
func (s Session) fields() []any {
	return []any{"uid", s.UserID, "sid", s.ID, "method", s.Method, "ip", s.IP, "ua", s.UserAgent,
		"tv", s.TokenVersion, "created_at", ms(s.CreatedAt), "last_seen", ms(s.LastSeen)}
}

// parseSession reads a session HASH; ok is false for a partial one.
func parseSession(f map[string]string) (Session, bool) {
	uid, _ := strconv.ParseInt(f["uid"], 10, 64)
	tv, _ := strconv.Atoi(f["tv"])
	created, _ := strconv.ParseInt(f["created_at"], 10, 64)
	seen, _ := strconv.ParseInt(f["last_seen"], 10, 64)
	s := Session{ID: f["sid"], UserID: uid, Method: f["method"], IP: f["ip"], UserAgent: f["ua"], TokenVersion: tv,
		CreatedAt: time.UnixMilli(created), LastSeen: time.UnixMilli(seen)}
	return s, uid > 0 && s.ID != "" && created > 0
}

// dropLua defines drop(p, sk): it ends the session whose sid key is sk, the
// current token and the one a rotation left in its grace (prev).
const dropLua = `
local function drop(p, sk)
  local h = redis.call('GET', sk)
  if h then
    local k = p .. 'sess:' .. h
    local prev = redis.call('HGET', k, 'prev')
    if prev then redis.call('DEL', p .. 'sess:' .. prev) end
    redis.call('DEL', k)
  end
  redis.call('DEL', sk)
end
`

// createScript stores a new token of a session. A session already under
// the id (a retry of a create whose reply was lost) ends first, so no
// token of it is left that only a token_version bump could end. The user's index
// forgets the ids idle past the timeout, and the sessions used least
// recently past MaxSessions end.
// KEYS: sess, sid, usess. ARGV: prefix, hash, sid, ttl ms, now ms, idle
// cutoff ms, usess ttl ms, max sessions, the fields...
var createScript = redis.NewScript(dropLua + `
local p = ARGV[1]
drop(p, KEYS[2])
redis.call('HSET', KEYS[1], unpack(ARGV, 9))
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[4])
redis.call('ZADD', KEYS[3], ARGV[5], ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', '(' .. ARGV[6])
redis.call('PEXPIRE', KEYS[3], ARGV[7])
local over = redis.call('ZCARD', KEYS[3]) - tonumber(ARGV[8])
if over > 0 then
  for _, id in ipairs(redis.call('ZRANGE', KEYS[3], 0, over - 1)) do
    drop(p, p .. 'sid:' .. id)
    redis.call('ZREM', KEYS[3], id)
  end
end
return 1`)

// Create implements SessionStore.
func (v *Valkey) Create(ctx context.Context, s Session) (string, error) {
	token, h := NewToken(SessionPrefix)
	now := v.o.Now()
	s.CreatedAt, s.LastSeen = now, now
	args := append([]any{keyPrefix, h, s.ID, v.ttl(s, now).Milliseconds(), ms(now), ms(now.Add(-v.o.Idle)),
		v.o.Max.Milliseconds(), v.o.MaxSessions}, s.fields()...)
	if err := createScript.Run(ctx, v.rdb, []string{sessKey(h), sidKey(s.ID), userKey(s.UserID)}, args...).Err(); err != nil {
		return "", fail(ctx, "create", err)
	}
	return token, nil
}

// Lookup implements SessionStore.
func (v *Valkey) Lookup(ctx context.Context, token string) (Session, error) {
	c, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	f, err := v.rdb.HGetAll(c, sessKey(Hash(token))).Result()
	if err != nil {
		return Session{}, fail(ctx, "lookup", err)
	}
	s, ok := parseSession(f)
	// The key's TTL ends it at the absolute timeout too; checking here does
	// not depend on when Valkey gets to expire it.
	if !ok || !v.o.Now().Before(s.CreatedAt.Add(v.o.Max)) {
		return Session{}, ErrNotFound
	}
	return s, nil
}

// touchScript extends a session that is still its id's current token, and
// records where it was used from: a revoked one (sid key gone) or a rotated
// one (sid points elsewhere) is not brought back, and neither is one that
// expired since it was read (no partial HASH is written).
// KEYS: sess, sid, usess. ARGV: hash, now ms, ttl ms, sid, usess ttl ms,
// ip, ua.
var touchScript = redis.NewScript(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] or redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
redis.call('HSET', KEYS[1], 'last_seen', ARGV[2], 'ip', ARGV[6], 'ua', ARGV[7])
redis.call('PEXPIRE', KEYS[1], ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
redis.call('ZADD', KEYS[3], ARGV[2], ARGV[4])
redis.call('PEXPIRE', KEYS[3], ARGV[5])
return 1`)

// Touch implements SessionStore: most requests only read, a use is written
// once per TouchEvery. Where it was used from goes with it, so the sessions
// list shows where a session is in use now, not only where it signed in.
func (v *Valkey) Touch(ctx context.Context, token string, s Session) error {
	now := v.o.Now()
	if now.Sub(s.LastSeen) < v.o.TouchEvery {
		return nil
	}
	ttl := v.ttl(s, now)
	if ttl <= 0 {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	h := Hash(token)
	err := touchScript.Run(c, v.rdb, []string{sessKey(h), sidKey(s.ID), userKey(s.UserID)},
		h, ms(now), ttl.Milliseconds(), s.ID, v.o.Max.Milliseconds(), s.IP, s.UserAgent).Err()
	if err != nil {
		return fail(ctx, "touch", err)
	}
	return nil
}

// rotateScript copies the session the id's current token names to a new
// token and points the id at it; the old token lives on for the grace (or
// less), and the new one remembers it (prev) so a revocation ends both. A
// token the previous rotation left in its grace ends now: only one old
// token is ever left behind. The id already at the new token is this
// rotation retried (its answer was lost): done, and doing it again would
// end the old token and cut the new one to the grace. The id at another
// token than the presented one was rotated since the caller's request was
// sent (another tab verified first): rotating again would end the token
// that tab took up for one nobody receives but this caller, so it is left
// (2), and the caller takes up the newer token from its tab.
// KEYS: sid. ARGV: prefix, uid, new hash, now ms, grace ms, presented hash.
var rotateScript = redis.NewScript(`
local old = redis.call('GET', KEYS[1])
if not old then return 0 end
local oldKey = ARGV[1] .. 'sess:' .. old
if redis.call('HGET', oldKey, 'uid') ~= ARGV[2] then return 0 end
if old == ARGV[3] then return 1 end
if old ~= ARGV[6] then return 2 end
local ttl = redis.call('PTTL', oldKey)
if ttl <= 0 then return 0 end
local older = redis.call('HGET', oldKey, 'prev')
if older then
  redis.call('DEL', ARGV[1] .. 'sess:' .. older)
  redis.call('HDEL', oldKey, 'prev')
end
local newKey = ARGV[1] .. 'sess:' .. ARGV[3]
redis.call('HSET', newKey, unpack(redis.call('HGETALL', oldKey)))
redis.call('HSET', newKey, 'prev', old, 'last_seen', ARGV[4])
redis.call('PEXPIRE', newKey, ttl)
redis.call('SET', KEYS[1], ARGV[3], 'PX', ttl)
redis.call('PEXPIRE', oldKey, math.min(ttl, tonumber(ARGV[5])))
return 1`)

// Rotate implements SessionStore.
func (v *Valkey) Rotate(ctx context.Context, userID int64, id, presented string) (string, error) {
	token, h := NewToken(SessionPrefix)
	n, err := rotateScript.Run(ctx, v.rdb, []string{sidKey(id)},
		keyPrefix, userID, h, ms(v.o.Now()), v.o.RotateGrace.Milliseconds(), Hash(presented)).Int()
	switch {
	case err != nil:
		return "", fail(ctx, "rotate", err)
	case n == 0:
		return "", ErrNotFound
	case n == 2:
		return "", ErrSuperseded
	}
	return token, nil
}

// listScript returns the HASH of each session in the user's index, most
// recently used first, and drops the ids whose session is gone (expired
// keys leave their index entry behind).
// KEYS: usess. ARGV: prefix.
var listScript = redis.NewScript(`
local out = {}
for _, id in ipairs(redis.call('ZREVRANGE', KEYS[1], 0, -1)) do
  local h = redis.call('GET', ARGV[1] .. 'sid:' .. id)
  local f = {}
  if h then f = redis.call('HGETALL', ARGV[1] .. 'sess:' .. h) end
  if #f == 0 then redis.call('ZREM', KEYS[1], id) else table.insert(out, f) end
end
return out`)

// List implements SessionStore.
func (v *Valkey) List(ctx context.Context, userID int64) ([]Session, error) {
	res, err := listScript.Run(ctx, v.rdb, []string{userKey(userID)}, keyPrefix).Slice()
	if err != nil {
		return nil, fail(ctx, "list", err)
	}
	out := make([]Session, 0, len(res))
	for _, r := range res {
		flat, _ := r.([]any)
		f := make(map[string]string, len(flat)/2)
		for i := 0; i+1 < len(flat); i += 2 {
			k, _ := flat[i].(string)
			f[k], _ = flat[i+1].(string)
		}
		if s, ok := parseSession(f); ok && s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

// hasScript reports whether the id names a live session of the user
// stamped the version or above.
// KEYS: sid. ARGV: prefix, uid, version.
var hasScript = redis.NewScript(`
local h = redis.call('GET', KEYS[1])
if not h then return 0 end
local f = redis.call('HMGET', ARGV[1] .. 'sess:' .. h, 'uid', 'tv')
if f[1] == ARGV[2] and (tonumber(f[2]) or 0) >= tonumber(ARGV[3]) then return 1 end
return 0`)

// Has implements SessionStore.
func (v *Valkey) Has(ctx context.Context, userID int64, id string, tokenVersion int) (bool, error) {
	n, err := hasScript.Run(ctx, v.rdb, []string{sidKey(id)}, keyPrefix, userID, tokenVersion).Int()
	if err != nil {
		return false, fail(ctx, "has", err)
	}
	return n == 1, nil
}

// revokeScript ends the picked sessions of the user (both tokens of a
// rotation): Only, or every one but Keep. With a bump from..to it also
// settles the others: one stamped below from was ended by an earlier bump
// and ends now, one below to is stamped to (both tokens too: a request
// still carrying the old one must not find it stale). Nothing stamped below
// from is ever stamped again, so no bump revives a session an earlier one
// ended. With a bump, a session issued after the call (later) is left
// alone: the script may run long after it, Valkey having held it past the
// caller's timeout. The index entries whose session is gone are dropped.
// Only is tried even when the index lost it; with no bump (to 0), it is
// the only session read. A session is ended only when it is the user's.
// KEYS: usess. ARGV: prefix, uid, only, keep, from, to, now ms.
var revokeScript = redis.NewScript(dropLua + `
local p, uid, only, keep = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local from, to, now = tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7])
-- settle stamps the session key k up to to; false when it is stamped
-- below from, ended.
local function settle(k)
  local tv = tonumber(redis.call('HGET', k, 'tv')) or 0
  if tv < from then return false end
  if tv < to then redis.call('HSET', k, 'tv', to) end
  return true
end
-- later tells a session issued after the call. No session from before it
-- is stamped above to (a stamp is never more than one ahead of the
-- version, from), so one above signed in after the caller fell back to a
-- stampless bump that committed. Nor was one from before created after
-- now, so one stamped below to and created since signed in after the
-- caller's change rolled back. One stamped to is never spared: a clock
-- alone (another node's, ahead) then spares nothing the bump does not end.
-- Keep is never spared, nor anything without a bump.
local function later(id, k)
  if to == 0 or id == keep then return false end
  local f = redis.call('HMGET', k, 'tv', 'created_at')
  local tv = tonumber(f[1]) or 0
  return tv > to or (tv < to and (tonumber(f[2]) or 0) > now)
end
local ids = {}
if only == '' or to > 0 then ids = redis.call('ZRANGE', KEYS[1], 0, -1) end
if only ~= '' then table.insert(ids, only) end
for _, id in ipairs(ids) do
  local sid = p .. 'sid:' .. id
  local h = redis.call('GET', sid)
  local k = h and (p .. 'sess:' .. h)
  if not h or redis.call('HGET', k, 'uid') ~= uid then
    redis.call('ZREM', KEYS[1], id)
  elseif later(id, k) then
    -- left as it is
  elseif ((only == '' or id == only) and id ~= keep) or not settle(k) then
    drop(p, sid)
    redis.call('ZREM', KEYS[1], id)
  else
    local prev = redis.call('HGET', k, 'prev')
    local pk = prev and (p .. 'sess:' .. prev)
    if pk and redis.call('EXISTS', pk) == 1 and not settle(pk) then redis.call('DEL', pk) end
  end
end
return 1`)

// Revoke implements SessionStore.
func (v *Valkey) Revoke(ctx context.Context, userID int64, sel Selection, b Bump) error {
	err := revokeScript.Run(ctx, v.rdb, []string{userKey(userID)}, keyPrefix, userID, sel.Only, sel.Keep, b.From, b.To, ms(v.o.Now())).Err()
	if err != nil {
		return fail(ctx, "revoke", err)
	}
	return nil
}

// pruneScript ends the sessions of the user stamped below the version (and
// a token a rotation left behind stamped below it), and drops the index
// entries whose session is gone. It stamps nothing, so a session issued
// since, stamped the version or above, stays whenever it runs.
// KEYS: usess. ARGV: prefix, uid, version.
var pruneScript = redis.NewScript(dropLua + `
local p, uid, version = ARGV[1], ARGV[2], tonumber(ARGV[3])
local function ended(k) return (tonumber(redis.call('HGET', k, 'tv')) or 0) < version end
for _, id in ipairs(redis.call('ZRANGE', KEYS[1], 0, -1)) do
  local sid = p .. 'sid:' .. id
  local h = redis.call('GET', sid)
  local k = h and (p .. 'sess:' .. h)
  if not h or redis.call('HGET', k, 'uid') ~= uid then
    redis.call('ZREM', KEYS[1], id)
  elseif ended(k) then
    drop(p, sid)
    redis.call('ZREM', KEYS[1], id)
  else
    local prev = redis.call('HGET', k, 'prev')
    if prev and ended(p .. 'sess:' .. prev) then redis.call('DEL', p .. 'sess:' .. prev) end
  end
end
return 1`)

// Prune implements SessionStore.
func (v *Valkey) Prune(ctx context.Context, userID int64, tokenVersion int) error {
	if err := pruneScript.Run(ctx, v.rdb, []string{userKey(userID)}, keyPrefix, userID, tokenVersion).Err(); err != nil {
		return fail(ctx, "prune", err)
	}
	return nil
}

// dropScript ends what one token stamped below the version still opens
// (Drop); 0 when it is stamped the version or above by now.
// KEYS: sess, sid, usess. ARGV: prefix, hash, sid, version.
var dropScript = redis.NewScript(dropLua + `
if (tonumber(redis.call('HGET', KEYS[1], 'tv')) or 0) >= tonumber(ARGV[4]) then return 0 end
if redis.call('GET', KEYS[2]) == ARGV[2] then
  drop(ARGV[1], KEYS[2])
  redis.call('ZREM', KEYS[3], ARGV[3])
else
  redis.call('DEL', KEYS[1])
end
return 1`)

// Drop implements SessionStore.
func (v *Valkey) Drop(ctx context.Context, token string, s Session, version int) (bool, error) {
	c, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	h := Hash(token)
	n, err := dropScript.Run(c, v.rdb, []string{sessKey(h), sidKey(s.ID), userKey(s.UserID)}, keyPrefix, h, s.ID, version).Int()
	if err != nil {
		return false, fail(ctx, "drop", err)
	}
	return n == 1, nil
}

// ------------------------------------------------------------ flows

// flowRecord is a flow as stored.
type flowRecord struct {
	UserID  int64           `json:"u,omitempty"`
	Payload json.RawMessage `json:"p"`
}

// PutFlow implements FlowStore.
func (v *Valkey) PutFlow(ctx context.Context, purpose string, f Flow) (string, error) {
	raw, err := json.Marshal(flowRecord{UserID: f.UserID, Payload: f.Payload})
	if err != nil {
		return "", err
	}
	token, h := NewToken("")
	if err := v.rdb.Set(ctx, flowKey(purpose, h), raw, v.o.FlowTTL).Err(); err != nil {
		return "", fail(ctx, "put flow", err)
	}
	return token, nil
}

// PeekFlow implements FlowStore.
func (v *Valkey) PeekFlow(ctx context.Context, purpose, token string) (Flow, error) {
	return v.readFlow(ctx, "peek flow", v.rdb.Get(ctx, flowKey(purpose, Hash(token))))
}

// TakeFlow implements FlowStore.
func (v *Valkey) TakeFlow(ctx context.Context, purpose, token string) (Flow, error) {
	return v.readFlow(ctx, "take flow", v.rdb.GetDel(ctx, flowKey(purpose, Hash(token))))
}

func (v *Valkey) readFlow(ctx context.Context, op string, cmd *redis.StringCmd) (Flow, error) {
	raw, err := cmd.Bytes()
	if err != nil {
		return Flow{}, fail(ctx, op, err)
	}
	var r flowRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return Flow{}, ErrNotFound // not written by PutFlow
	}
	return Flow{UserID: r.UserID, Payload: r.Payload}, nil
}

// OpenWindow implements FlowStore: the value is when it was opened.
func (v *Valkey) OpenWindow(ctx context.Context, name string, idle time.Duration) error {
	if err := v.rdb.Set(ctx, windowKey(name), ms(v.o.Now()), idle).Err(); err != nil {
		return fail(ctx, "open window", err)
	}
	return nil
}

// continueScript extends an open window to idle, capped at max after it
// was opened. KEYS: window. ARGV: now ms, idle ms, max ms.
var continueScript = redis.NewScript(`
local opened = tonumber(redis.call('GET', KEYS[1]))
if not opened then return 0 end
local left = opened + tonumber(ARGV[3]) - tonumber(ARGV[1])
if left <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
redis.call('PEXPIRE', KEYS[1], math.min(tonumber(ARGV[2]), left))
return 1`)

// ContinueWindow implements FlowStore.
func (v *Valkey) ContinueWindow(ctx context.Context, name string, idle, max time.Duration) (bool, error) {
	n, err := continueScript.Run(ctx, v.rdb, []string{windowKey(name)}, ms(v.o.Now()), idle.Milliseconds(), max.Milliseconds()).Int()
	if err != nil {
		return false, fail(ctx, "continue window", err)
	}
	return n == 1, nil
}

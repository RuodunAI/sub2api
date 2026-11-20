-- moderation 0001: moderation records, bans and unbans (CONTRACTS §20.6).
-- Runs as the plugin's database role in the core's schema: every table,
-- index and sequence is named plg_moderation__<name> (CONTRACTS §130).

-- One row per moderated request (pass verdicts only with record_pass).
CREATE TABLE IF NOT EXISTS plg_moderation__events (
    id                bigserial   PRIMARY KEY,
    created_at        timestamptz NOT NULL DEFAULT now(),
    request_id        text        NOT NULL DEFAULT '',
    user_id           bigint      NOT NULL DEFAULT 0,
    api_key_id        bigint      NOT NULL DEFAULT 0,
    group_id          bigint      NOT NULL DEFAULT 0,
    model             text        NOT NULL DEFAULT '',
    protocol          text        NOT NULL DEFAULT '',
    mode              text        NOT NULL CHECK (mode IN ('observe', 'enforce')),
    verdict           text        NOT NULL CHECK (verdict IN ('pass', 'flag', 'block', 'error')),
    action            text        NOT NULL CHECK (action IN ('allow', 'deny')),
    categories        text[]      NOT NULL DEFAULT '{}',
    severity          text        NOT NULL DEFAULT '',
    reason            text        NOT NULL DEFAULT '',
    error             text        NOT NULL DEFAULT '',
    text              text,       -- NULL when store_text is off
    text_chars        integer     NOT NULL DEFAULT 0,
    text_hash         text        NOT NULL DEFAULT '',
    cached            boolean     NOT NULL DEFAULT false,
    llm_model         text        NOT NULL DEFAULT '',
    turns             integer     NOT NULL DEFAULT 0,
    latency_ms        integer     NOT NULL DEFAULT 0,
    prompt_tokens     integer     NOT NULL DEFAULT 0,
    completion_tokens integer     NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS plg_moderation__events_created_at_idx ON plg_moderation__events (created_at);
CREATE INDEX IF NOT EXISTS plg_moderation__events_verdict_created_at_idx ON plg_moderation__events (verdict, created_at);
CREATE INDEX IF NOT EXISTS plg_moderation__events_user_created_at_idx ON plg_moderation__events (user_id, created_at);

-- Banned users (automatic after ban_threshold violations, or manual).
CREATE TABLE IF NOT EXISTS plg_moderation__blocks (
    user_id    bigint      PRIMARY KEY,
    reason     text        NOT NULL DEFAULT '',
    violations integer     NOT NULL DEFAULT 0,
    source     text        NOT NULL CHECK (source IN ('auto', 'manual')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,          -- NULL = until unblocked
    created_by bigint
);
CREATE INDEX IF NOT EXISTS plg_moderation__blocks_expires_at_idx ON plg_moderation__blocks (expires_at);

-- Last unblock per user: violations are only counted after it.
CREATE TABLE IF NOT EXISTS plg_moderation__unblocks (
    user_id bigint      PRIMARY KEY,
    at      timestamptz NOT NULL
);

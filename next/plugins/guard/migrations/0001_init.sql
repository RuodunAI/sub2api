-- guard 0001: rules, statistics and event idempotency.
-- Runs as the plugin's database role in the core's schema: every table,
-- index and sequence is named plg_guard__<name> (CONTRACTS §130).

CREATE TABLE IF NOT EXISTS plg_guard__rules (
    id         bigserial    PRIMARY KEY,
    name       varchar(100) NOT NULL,
    kind       varchar(20)  NOT NULL CHECK (kind IN ('keyword', 'regex')),
    pattern    text         NOT NULL,
    enabled    boolean      NOT NULL DEFAULT true,
    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now()
);

-- Demo rule so a fresh install can be tested end to end.
INSERT INTO plg_guard__rules (name, kind, pattern, enabled)
SELECT 'demo: test marker', 'keyword', 'GUARD_TEST_BLOCK', true
WHERE NOT EXISTS (SELECT 1 FROM plg_guard__rules WHERE name = 'demo: test marker');

-- One row per blocked request (snippet only when record_snippets is on).
CREATE TABLE IF NOT EXISTS plg_guard__block_log (
    id          bigserial    PRIMARY KEY,
    occurred_at timestamptz  NOT NULL,
    rule_id     bigint       NOT NULL,
    rule_name   varchar(100) NOT NULL,
    request_id  varchar(100) NOT NULL DEFAULT '',
    user_id     bigint       NOT NULL DEFAULT 0,
    group_id    bigint       NOT NULL DEFAULT 0,
    model       varchar(200) NOT NULL DEFAULT '',
    snippet     text
);
CREATE INDEX IF NOT EXISTS plg_guard__block_log_occurred_at_idx ON plg_guard__block_log (occurred_at);

-- requests: from usage.recorded events; blocked: from the hook.
CREATE TABLE IF NOT EXISTS plg_guard__stats_minutely (
    minute   timestamptz  NOT NULL,
    group_id bigint       NOT NULL,
    model    varchar(200) NOT NULL,
    requests bigint       NOT NULL DEFAULT 0,
    blocked  bigint       NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, group_id, model)
);

CREATE TABLE IF NOT EXISTS plg_guard__stats_hourly (
    hour     timestamptz  NOT NULL,
    group_id bigint       NOT NULL,
    model    varchar(200) NOT NULL,
    requests bigint       NOT NULL DEFAULT 0,
    blocked  bigint       NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, group_id, model)
);

-- Event ids already applied (at-least-once delivery).
CREATE TABLE IF NOT EXISTS plg_guard__processed_events (
    event_id     bigint      PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS plg_guard__processed_events_processed_at_idx ON plg_guard__processed_events (processed_at);

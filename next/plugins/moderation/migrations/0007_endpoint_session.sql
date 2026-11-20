-- moderation 0007: moderation 0.5.0 (CONTRACTS §72.14): the client
-- endpoint (path pattern, e.g. /v1/messages) and the sticky-session
-- fingerprint of each record, from RequestMeta.endpoint and
-- session_fingerprint, so records can be filtered by endpoint and session
-- (new-api moderation_records.endpoint / session_fp). Idempotent; the columns
-- have defaults so older versions ignore them.

ALTER TABLE plg_moderation__events ADD COLUMN IF NOT EXISTS endpoint   text NOT NULL DEFAULT '';
ALTER TABLE plg_moderation__events ADD COLUMN IF NOT EXISTS session_fp text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS plg_moderation__events_session_fp_idx ON plg_moderation__events (session_fp, created_at) WHERE session_fp <> '';

-- Console sessions live in Valkey only (CONTRACTS §100): the JWT refresh
-- token families, their login session rows and the auth flows have no
-- reader left, and neither has the session.issue switch (settings row
-- "session"). Dropping them signs out every login made before this
-- version, which the release takes (deploy/rollback/0.1.290-session-down.md:
-- stop every node first, one way, back only from a backup).
--
-- Safe to run again: IF EXISTS, and a DELETE that finds nothing.

SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS login_sessions;
DROP TABLE IF EXISTS auth_flows;
DELETE FROM settings WHERE key = 'session';

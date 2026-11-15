-- Reverses core migration 0100_casbin_rule.sql (core 0.1.290, a candidate
-- version: see 0.1.290-casbin-down.md): puts the database back in the shape
-- core 0.1.289 runs on (migrations through 0099). Run it with every node
-- stopped, as the database owner, in one go:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f 0.1.290-casbin-down.sql
--
-- The migration only added things and never rewrote the role tables, which
-- stay the source of truth, so nothing is lost: the triggers, functions,
-- views and casbin_rule go, and so does the schema_migrations row. The authz
-- version moves.

BEGIN;

SET LOCAL lock_timeout = '10s';

SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE id = '0100_casbin_rule.sql') THEN
        RAISE EXCEPTION 'migration 0100_casbin_rule.sql is not applied: nothing to roll back';
    END IF;
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE id > '0100_casbin_rule.sql') THEN
        RAISE EXCEPTION 'migrations after 0100_casbin_rule.sql are applied: roll those back first';
    END IF;
END $$;

DROP TRIGGER IF EXISTS casbin_sync ON role_permissions;
DROP TRIGGER IF EXISTS casbin_sync ON role_permission_scopes;
DROP TRIGGER IF EXISTS casbin_sync ON user_roles;
DROP TRIGGER IF EXISTS casbin_sync ON roles;
DROP TRIGGER IF EXISTS casbin_sync ON permissions;
DROP TRIGGER IF EXISTS casbin_sync ON users;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permissions;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permission_scopes;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON user_roles;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON roles;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON permissions;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON users;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON casbin_rule;

DROP FUNCTION IF EXISTS authz_casbin_on_grant();
DROP FUNCTION IF EXISTS authz_casbin_on_user_role();
DROP FUNCTION IF EXISTS authz_casbin_on_role();
DROP FUNCTION IF EXISTS authz_casbin_on_permission();
DROP FUNCTION IF EXISTS authz_casbin_on_user();
DROP FUNCTION IF EXISTS authz_casbin_on_truncate();
DROP FUNCTION IF EXISTS authz_casbin_sync_grant(bigint, bigint, text);
DROP FUNCTION IF EXISTS authz_casbin_sync_user_role(bigint, bigint);
DROP FUNCTION IF EXISTS authz_casbin_set(text, text, text, text, boolean);
DROP FUNCTION IF EXISTS authz_casbin_repair();

DROP VIEW IF EXISTS authz_casbin_drift;
DROP VIEW IF EXISTS authz_casbin_expected;
DROP TABLE IF EXISTS casbin_rule;

DELETE FROM schema_migrations WHERE id = '0100_casbin_rule.sql';

UPDATE authz_meta SET version = version + 1 WHERE id = 1;

COMMIT;

-- Reverses core migration 0100_casbin_rule.sql (core 0.1.290, a candidate
-- version: see 0.1.290-casbin-down.md): puts the database back in the shape
-- core 0.1.289 runs on (migrations through 0099). Run it with every node
-- stopped, as the database owner, in one go:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f 0.1.290-casbin-down.sql
--
-- The migration only added things and never rewrote the role tables, which
-- stay the source of truth, so nothing is lost: the triggers, functions,
-- views and casbin_rule go, 0097's user_roles_one_superuser gets its
-- search_path back, the authz.engine setting goes, and so does the
-- schema_migrations row. The authz version moves.

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

DROP TRIGGER IF EXISTS casbin_sync ON permissions;
DROP TRIGGER IF EXISTS casbin_sync ON roles;
DROP TRIGGER IF EXISTS casbin_sync ON role_permissions;
DROP TRIGGER IF EXISTS casbin_sync ON role_permission_scopes;
DROP TRIGGER IF EXISTS casbin_sync ON user_roles;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON permissions;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON roles;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permissions;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permission_scopes;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON user_roles;
DROP TRIGGER IF EXISTS casbin_sync_truncate ON casbin_rule;

DROP FUNCTION IF EXISTS authz_casbin_on_grant();
DROP FUNCTION IF EXISTS authz_casbin_on_user_role();
DROP FUNCTION IF EXISTS authz_casbin_on_role();
DROP FUNCTION IF EXISTS authz_casbin_on_permission();
DROP FUNCTION IF EXISTS authz_casbin_on_truncate();
DROP FUNCTION IF EXISTS authz_casbin_sync_grant(bigint, bigint, text);
DROP FUNCTION IF EXISTS authz_casbin_sync_user_role(bigint, bigint);
DROP FUNCTION IF EXISTS authz_casbin_set(text, text, text, text, boolean);
DROP FUNCTION IF EXISTS authz_casbin_check_role_key(text);
DROP FUNCTION IF EXISTS authz_casbin_check_permission_key(text);
DROP FUNCTION IF EXISTS authz_casbin_repair();

ALTER FUNCTION user_roles_one_superuser() RESET search_path;

DROP VIEW IF EXISTS authz_casbin_drift;
DROP VIEW IF EXISTS authz_casbin_expected;
DROP TABLE IF EXISTS casbin_rule;
-- The mapping's functions, once nothing that calls them is left.
DROP FUNCTION IF EXISTS authz_casbin_user_sub(bigint);
DROP FUNCTION IF EXISTS authz_casbin_role_sub(text);
DROP FUNCTION IF EXISTS authz_casbin_super_key();
DROP FUNCTION IF EXISTS authz_casbin_all_rows();

-- The authz.engine setting (CONTRACTS §99.3): 0.1.289 does not read it, and
-- left behind it would start a later upgrade on the engine chosen before,
-- casbin perhaps, without the shadow period of §99.5.
DELETE FROM settings WHERE key = 'authz';

DELETE FROM schema_migrations WHERE id = '0100_casbin_rule.sql';

UPDATE authz_meta SET version = version + 1 WHERE id = 1;

COMMIT;

-- Reverses core migration 0099_plugin_permission_meta.sql (core 0.1.289):
-- puts the database back in the shape core 0.1.288 runs on (migrations
-- through 0098). Run it with every node stopped, as the database owner, in
-- one go:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f 0.1.289-plugin-perm-down.sql
--
-- What it does, in one transaction (see 0.1.289-plugin-perm-down.md):
--   1. Plugin permissions no longer declared (status 'removed') are deleted
--      with their grants: core 0.1.288's SetPluginActive would make them
--      active again, and deleting them is what it did itself.
--   2. Scoped grants of plugin permissions are deleted: core 0.1.288 cannot
--      parse plugin scopes, so they granted nothing there.
--   3. Schema: the columns and constraint the migration added go,
--      scope_type is narrowed back (plugin rows cleared first), and so does
--      its schema_migrations row.
-- Every dropped grant is audited by the system with via "rollback 0.1.289";
-- the authz version moves.

BEGIN;

SET LOCAL lock_timeout = '10s';

SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE id = '0099_plugin_permission_meta.sql') THEN
        RAISE EXCEPTION 'migration 0099_plugin_permission_meta.sql is not applied: nothing to roll back';
    END IF;
    IF EXISTS (SELECT 1 FROM schema_migrations WHERE id > '0099_plugin_permission_meta.sql') THEN
        RAISE EXCEPTION 'migrations after 0099_plugin_permission_meta.sql are applied: roll those back first';
    END IF;
END $$;

-- 1 and 2. Every grant that goes: each grant of a removed plugin permission
-- and each scoped grant of a plugin permission.
INSERT INTO audit_logs (user_id, action, target_type, target_id, detail)
SELECT NULL, 'authz.grant.migrate', 'role', g.role_id::text, jsonb_build_object('via', 'rollback 0.1.289', 'dropped', g.dropped)
FROM (
    SELECT rp.role_id, p.key AS dropped FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id
    WHERE p.source = 'plugin' AND p.status = 'removed'
    UNION ALL
    SELECT rs.role_id, p.key || ':' || rs.scope FROM role_permission_scopes rs JOIN permissions p ON p.id = rs.permission_id
    WHERE p.source = 'plugin'
) g;

DELETE FROM permissions WHERE source = 'plugin' AND status = 'removed';
DELETE FROM role_permission_scopes rs USING permissions p WHERE p.id = rs.permission_id AND p.source = 'plugin';

-- 3. Schema. Only plugin rows can have a scope_type over 20 characters, and
-- core 0.1.288 gives plugin permissions none.
ALTER TABLE permissions DROP CONSTRAINT IF EXISTS permissions_risk_check;
ALTER TABLE permissions DROP COLUMN IF EXISTS risk;
ALTER TABLE permissions DROP COLUMN IF EXISTS requires;
ALTER TABLE permissions DROP COLUMN IF EXISTS group_key;
ALTER TABLE permissions DROP COLUMN IF EXISTS group_label;
ALTER TABLE permissions DROP COLUMN IF EXISTS group_sort;
ALTER TABLE permissions DROP COLUMN IF EXISTS scope;
ALTER TABLE permissions DROP COLUMN IF EXISTS defaults;
UPDATE permissions SET scope_type = '' WHERE source = 'plugin' AND scope_type <> '';
ALTER TABLE permissions ALTER COLUMN scope_type TYPE varchar(20);
COMMENT ON COLUMN permissions.status IS NULL;
DELETE FROM schema_migrations WHERE id = '0099_plugin_permission_meta.sql';

UPDATE authz_meta SET version = version + 1 WHERE id = 1;

COMMIT;

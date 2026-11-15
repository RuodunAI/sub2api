-- Reverses core migration 0099_plugin_permission_meta.sql (core 0.1.289):
-- puts the database back in the shape core 0.1.288 runs on (migrations
-- through 0098). Run it with every node stopped, as the database owner, in
-- one go:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f 0.1.289-plugin-perm-down.sql
--
-- What it does, in one transaction (see 0.1.289-plugin-perm-down.md):
--   0. Withheld role grants core 0.1.288 would confirm wrong (a scoped or a
--      critical one) are rejected, so that its review list leaves them out.
--   1. Plugin permissions no longer declared (status 'removed') or declared
--      only by a version approved but never activated (status 'staged') are
--      deleted with their grants: core 0.1.288's SetPluginActive would make
--      them active, and deleting no longer declared keys is what it did
--      itself.
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

-- 0. Core 0.1.288 lists withheld role grants (plugin.consent.auto
-- detail.pending) by permission and role alone: confirming a scoped one
-- would grant every row, and it knows no critical risk, so a non-superuser
-- could confirm one. Each such grant still awaiting review (0.1.289's rule,
-- removed permissions aside) is rejected, one audit per plugin; both cores
-- read a reject's grants as clearing that (permission, role) under every
-- version recorded before it.
INSERT INTO audit_logs (user_id, action, target_type, target_id, detail)
SELECT NULL, 'plugin.role_grants.reject', 'plugin', w.target_id,
       jsonb_build_object('via', 'rollback 0.1.289', 'grants', jsonb_agg(DISTINCT w.item ORDER BY w.item))
FROM (
    SELECT a.target_id, jsonb_build_object('permission', g->>'permission', 'role', g->>'role',
           'scope', COALESCE(g->>'scope', ''), 'risk', p.risk) AS item
    FROM audit_logs a CROSS JOIN LATERAL jsonb_array_elements(a.detail->'pending') g
    JOIN permissions p ON p.key = g->>'permission' AND p.source = 'plugin' AND p.status <> 'removed'
    WHERE a.action = 'plugin.consent.auto' AND a.target_type = 'plugin'
      AND jsonb_typeof(a.detail->'pending') = 'array'
      AND a.created_at >= (SELECT installed_at FROM plugins WHERE key = a.target_id)
      AND (COALESCE(g->>'scope', '') <> '' OR p.risk = 'critical')
      AND NOT EXISTS (SELECT 1 FROM audit_logs b WHERE b.target_type = 'plugin' AND b.target_id = a.target_id
          AND b.action IN ('plugin.role_grants.confirm', 'plugin.role_grants.reject') AND b.id > a.id
          AND (b.detail->>'version' = a.detail->>'version'
            OR (b.action = 'plugin.role_grants.reject'
                AND b.detail->'grants' @> jsonb_build_array(jsonb_build_object('permission', g->>'permission', 'role', g->>'role')))))
) w
GROUP BY w.target_id;

-- 1 and 2. Every grant that goes: each grant of a removed or staged plugin
-- permission and each scoped grant of a plugin permission.
INSERT INTO audit_logs (user_id, action, target_type, target_id, detail)
SELECT NULL, 'authz.grant.migrate', 'role', g.role_id::text, jsonb_build_object('via', 'rollback 0.1.289', 'dropped', g.dropped)
FROM (
    SELECT rp.role_id, p.key AS dropped FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id
    WHERE p.source = 'plugin' AND p.status IN ('removed', 'staged')
    UNION ALL
    SELECT rs.role_id, p.key || ':' || rs.scope FROM role_permission_scopes rs JOIN permissions p ON p.id = rs.permission_id
    WHERE p.source = 'plugin'
) g;

DELETE FROM permissions WHERE source = 'plugin' AND status IN ('removed', 'staged');
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

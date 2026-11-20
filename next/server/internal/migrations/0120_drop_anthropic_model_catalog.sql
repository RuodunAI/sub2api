-- The model catalog of the built-in plugin anthropic (GET
-- /api/v1/p/anthropic/models, the user permission model_catalog:read, the
-- table plg_anthropic.model_catalog) is removed (core 0.1.304, CONTRACTS
-- §129). anthropic 0.4.0, bundled with this release, declares no database,
-- route or user permission, so nothing would drop what 0.3.x left behind:
-- this does, as dbschema.DropTx and authz.DeletePlugin would for a plugin
-- that is gone. The plugin itself stays installed; the core moves it to 0.4.0
-- after this release migrated. Its last catalog source is commit 1fbf3c470.
--
-- One way: the way back is the pre-upgrade backup. Safe to run again.

SET LOCAL lock_timeout = '10s';

-- The permission goes with its grants: casbin_rule names a permission by
-- key, so no foreign key cascades them, and the roles that held it move
-- their revision.
SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

WITH gone AS (
    DELETE FROM casbin_rule
    WHERE ptype = 'p' AND v1 = 'plugin.anthropic:model_catalog:read'
    RETURNING v0
)
UPDATE roles SET revision = revision + 1
WHERE 'r:' || key IN (SELECT v0 FROM gone);

UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1
WHERE id = 1 AND EXISTS (SELECT 1 FROM permissions WHERE key = 'plugin.anthropic:model_catalog:read');

DELETE FROM permissions WHERE key = 'plugin.anthropic:model_catalog:read';

-- The schema with its migration records (plugin_migrations has no foreign
-- key to plugins since 0002) and a legacy role of it carrying the core's
-- marker: the catalog was everything anthropic kept there.
DROP SCHEMA IF EXISTS plg_anthropic CASCADE;
DELETE FROM plugin_migrations WHERE plugin_key = 'anthropic';
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles
               WHERE rolname = 'plg_anthropic'
                 AND shobj_description(oid, 'pg_authid') =
                     'sub2api plugin role; database=' || current_database() || '; plugin=anthropic') THEN
        EXECUTE 'DROP OWNED BY plg_anthropic';
        EXECUTE 'DROP ROLE plg_anthropic';
    END IF;
END $$;

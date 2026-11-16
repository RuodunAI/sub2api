-- CCGateway is removed (0.1.290): the core module, the built-in plugin and
-- the gateway features only it used (provider files, skills and containers,
-- fallback credits, helper history). Its last source is commit 317b0c331.
-- This drops what the core kept for it and uninstalls the plugin the way
-- plugin/install.Uninstall does with purge, without the node barrier: every
-- node is stopped while a release migrates (primary first).
--
-- Kept on purpose:
--   - accounts with plugin_key 'ccgateway' (and their groups, usage rows,
--     ledger entries): they show as orphaned, like the accounts of any
--     uninstalled plugin; an administrator deletes them in the console;
--   - usage_logs, balance_ledger and audit_logs rows: billing history;
--   - plugin_executions, plugin_history and plugin_egress_logs rows: the
--     plugin's operating history, kept by install.Uninstall too;
--   - account_lifecycle_sync rows of those accounts: they go with the
--     accounts.
--
-- deploy/rollback/0.1.290-release.md lists what this removes and the
-- read-only counts to run before the upgrade. One way, like the rest of
-- 0.1.290: the way back is the pre-upgrade backup. Safe to run again.

SET LOCAL lock_timeout = '10s';

-- The tables only CCGateway wrote.
DROP TABLE IF EXISTS ccgateway_account_request_logs;
DROP TABLE IF EXISTS ccgateway_account_feature_policies;
DROP TABLE IF EXISTS ccgateway_runtimes;
DROP TABLE IF EXISTS provider_resource_contexts;
DROP TABLE IF EXISTS provider_skill_versions;
DROP TABLE IF EXISTS provider_resources;
DROP TABLE IF EXISTS provider_fallback_credits;
DROP TABLE IF EXISTS provider_diagnostic_messages;
DROP TABLE IF EXISTS provider_helper_usage_outbox;
DROP TABLE IF EXISTS provider_helper_usage_receipts;
DROP TABLE IF EXISTS provider_helper_records;
DROP TABLE IF EXISTS provider_helper_attempts;

-- Runtime parking (0085) and the tier the runtime reported (0086).
DROP TRIGGER IF EXISTS accounts_status_changed_at ON accounts;
DROP FUNCTION IF EXISTS accounts_status_changed_at();
ALTER TABLE accounts DROP COLUMN IF EXISTS status_changed_at;
ALTER TABLE accounts DROP COLUMN IF EXISTS rate_limit_tier;

-- The connection settings (controller address, keys, images, group CC
-- policies) and the core tasks of the removed modules.
DELETE FROM settings WHERE key = 'ccgateway_remote';
DELETE FROM scheduled_tasks
WHERE name IN ('core.ccgateway.draft_sweep', 'core.ccgateway.park_runtimes', 'core.ccgateway.runtime_sync',
               'core.helper.payload_expiry', 'core.helper.usage_replay')
   OR plugin_key = 'ccgateway';

-- Open account preparations of the plugin.
DELETE FROM account_preparations WHERE plugin_key = 'ccgateway';

-- The plugin itself (install.Uninstall with purge). Its permissions go with
-- their grants: casbin_rule names a permission by key, so no foreign key
-- cascades them (authz.DeletePlugin), and the roles that held one move
-- their revision.
SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

WITH gone AS (
    DELETE FROM casbin_rule c USING permissions p
    WHERE p.plugin_key = 'ccgateway' AND c.ptype = 'p' AND c.v1 = p.key
    RETURNING c.v0
)
UPDATE roles SET revision = revision + 1
WHERE 'r:' || key IN (SELECT v0 FROM gone);

DELETE FROM permissions WHERE plugin_key = 'ccgateway';

UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1
WHERE id = 1 AND EXISTS (SELECT 1 FROM plugins WHERE key = 'ccgateway');

-- The key stays with its publisher (install.retireKey), and the reservation
-- list keeps hand uploads off it.
INSERT INTO plugin_key_retirements (plugin_key, publisher_id)
SELECT key, publisher_id FROM plugins WHERE key = 'ccgateway'
ON CONFLICT (plugin_key) DO UPDATE SET publisher_id = EXCLUDED.publisher_id, retired_by = NULL, retired_at = now();

-- Cascades the versions (with their staged grants), the host permission
-- grants, event cursors and dead letters, job runs, rollouts, runtime nodes,
-- the uninstall barrier and sticky rules. Execution and egress logs stay,
-- like those of any uninstalled plugin.
DELETE FROM plugins WHERE key = 'ccgateway';
DELETE FROM plugin_egress_domains WHERE plugin_key = 'ccgateway';

-- CCGateway never declared a database permission, so there is no schema or
-- role to drop; the statements below only cover a hand-made one, as
-- dbschema.DropTx does (a role is ours only with its marker).
DROP SCHEMA IF EXISTS plg_ccgateway CASCADE;
DELETE FROM plugin_migrations WHERE plugin_key = 'ccgateway';
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles
               WHERE rolname = 'plg_ccgateway'
                 AND shobj_description(oid, 'pg_authid') =
                     'sub2api plugin role; database=' || current_database() || '; plugin=ccgateway') THEN
        EXECUTE 'DROP OWNED BY plg_ccgateway';
        EXECUTE 'DROP ROLE plg_ccgateway';
    END IF;
END $$;

-- The first-party plugin volcengine (Volcengine Ark: the account types apikey
-- and relay, the volcengine platform with Ark image generation and Seedance
-- video tasks, the Ark asset library) is removed (core 0.1.304, CONTRACTS
-- §128). It was a built-in, install-only plugin, so production databases
-- have it, some with accounts. Its last source is commit b58f8149e.
-- This uninstalls it the way plugin/install.Uninstall does with purge
-- (without the node barrier: every node is stopped while this release
-- migrates), as 0111 and 0117 did. It declared user permissions
-- (assets:read, assets:manage), so its permissions and grants go too; it
-- declared a database permission, so its schema plg_volcengine (the local
-- index of the asset library; the assets themselves stay at Volcengine) is
-- dropped with its migration records, and a legacy role of it carrying the
-- core's marker, as dbschema.DropTx does. It kept no KV entries and had no
-- jobs.
--
-- Kept on purpose:
--   - accounts with plugin_key 'volcengine' (and their groups, usage rows,
--     ledger entries, balance snapshots): they show as orphaned, like the
--     accounts of any uninstalled plugin; an administrator deletes them in
--     the console, one by one or all at once by releasing the retired key
--     with purge_accounts (POST /plugins/retired-keys/volcengine/release);
--   - async_tasks and pending_settlements rows of its video tasks still
--     being observed: the reconcile loop finds no platform service for the
--     plugin, which is a failed query like an account that can never be
--     asked again (CONTRACTS §112.9), so the failure budget
--     (max_poll_failures, 20 by default) closes each task as failed
--     (task_poll_failed) and refunds its reserve in the same transaction;
--     with the budget off (0) the task deadline does it (task_timeout). A
--     task already observed succeeded keeps its charge. Expired
--     task_submission_receipts are swept as usual;
--   - usage_logs, balance_ledger and audit_logs rows: billing history;
--   - plugin_executions, plugin_history and plugin_egress_logs rows: the
--     plugin's operating history, kept by install.Uninstall too.
--
-- One way: the way back is the pre-upgrade backup. Safe to run again.

SET LOCAL lock_timeout = '10s';

-- Tasks of its jobs (scheduled_tasks has no foreign key to plugins); it
-- declared none, this covers a hand-made one.
DELETE FROM scheduled_tasks WHERE plugin_key = 'volcengine';

-- Its permissions go with their grants: casbin_rule names a permission by
-- key, so no foreign key cascades them (authz.DeletePlugin), and the roles
-- that held one move their revision.
SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

WITH gone AS (
    DELETE FROM casbin_rule c USING permissions p
    WHERE p.plugin_key = 'volcengine' AND c.ptype = 'p' AND c.v1 = p.key
    RETURNING c.v0
)
UPDATE roles SET revision = revision + 1
WHERE 'r:' || key IN (SELECT v0 FROM gone);

DELETE FROM permissions WHERE plugin_key = 'volcengine';

UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1
WHERE id = 1 AND EXISTS (SELECT 1 FROM plugins WHERE key = 'volcengine');

-- The key stays with its publisher (install.retireKey); the removed keys
-- (install.removedKeys) refuse any new install of it.
INSERT INTO plugin_key_retirements (plugin_key, publisher_id)
SELECT key, publisher_id FROM plugins WHERE key = 'volcengine'
ON CONFLICT (plugin_key) DO UPDATE SET publisher_id = EXCLUDED.publisher_id, retired_by = NULL, retired_at = now();

-- Cascades the versions (with their staged grants), the host permission
-- grants, event cursors and dead letters, job runs, rollouts (with their
-- node results and cleanup barrier), runtime nodes, the uninstall barrier
-- and sticky rules.
DELETE FROM plugins WHERE key = 'volcengine';
DELETE FROM plugin_egress_domains WHERE plugin_key = 'volcengine';

-- The schema and its migration records (plugin_migrations has no foreign key
-- to plugins since 0002), as dbschema.DropTx drops them.
DROP SCHEMA IF EXISTS plg_volcengine CASCADE;
DELETE FROM plugin_migrations WHERE plugin_key = 'volcengine';
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles
               WHERE rolname = 'plg_volcengine'
                 AND shobj_description(oid, 'pg_authid') =
                     'sub2api plugin role; database=' || current_database() || '; plugin=volcengine') THEN
        EXECUTE 'DROP OWNED BY plg_volcengine';
        EXECUTE 'DROP ROLE plg_volcengine';
    END IF;
END $$;

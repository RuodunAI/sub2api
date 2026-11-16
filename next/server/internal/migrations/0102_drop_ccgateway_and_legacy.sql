-- CCGateway is removed (0.1.290): the core module, the built-in plugin and
-- the gateway features only it used (provider files, skills and containers,
-- fallback credits, helper history), and the account lifecycle contract it
-- was the only implementer of (preparations, runtime synchronization, the
-- subscription type and automatic models). Its last source is commit
-- 317b0c331.
-- This drops what the core kept for it, uninstalls the plugin the way
-- plugin/install.Uninstall does with purge (without the node barrier: every
-- node is stopped while a release migrates, primary first) and takes the
-- keywords only CCGateway answered with out of the failure policy. Last, it
-- drops the data kept only for the cores before 0.1.290 (CONTRACTS §101.4,
-- §101.6), which never run next to this one.
--
-- Kept on purpose:
--   - accounts with plugin_key 'ccgateway' (and their groups, usage rows,
--     ledger entries): they show as orphaned, like the accounts of any
--     uninstalled plugin; an administrator deletes them in the console;
--   - usage_logs, balance_ledger and audit_logs rows: billing history;
--   - plugin_executions, plugin_history and plugin_egress_logs rows: the
--     plugin's operating history, kept by install.Uninstall too.
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

-- The account lifecycle contract: account preparations (0051), runtime
-- synchronization (0051), the subscription type the runtime reported (0054)
-- and the automatic model switch handed to lifecycle hooks (0055).
DROP TABLE IF EXISTS account_preparations;
DROP TABLE IF EXISTS account_lifecycle_sync;
ALTER TABLE accounts DROP COLUMN IF EXISTS subscription_type;
ALTER TABLE accounts DROP COLUMN IF EXISTS auto_models;

-- The connection settings (controller address, keys, images, group CC
-- policies) and the core tasks of the removed modules.
DELETE FROM settings WHERE key = 'ccgateway_remote';
DELETE FROM scheduled_tasks
WHERE name IN ('core.ccgateway.draft_sweep', 'core.ccgateway.park_runtimes', 'core.ccgateway.runtime_sync',
               'core.helper.payload_expiry', 'core.helper.usage_replay',
               'core.account.lifecycle_status', 'core.account.preparation_cleanup')
   OR plugin_key = 'ccgateway';

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

-- The key stays with its publisher (install.retireKey); the removed keys
-- (install.removedKeys) refuse any new install of it.
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

-- The failure policy keywords only CCGateway answered with: its Worker's
-- codes (oauth_refresh_dead, oauth_account_on_hold, stored_login_refused_*),
-- its engine's messages (not_logged_in, "has cleared its stored login",
-- "gateway concurrency limit reached") and Claude Code's own client message
-- ("oauth refresh token is no longer valid"). A rule loses them only while
-- its keywords are exactly what 0072/0074 wrote (with or without what 0075
-- appended), or for rate-limit, the former default: a rule an administrator
-- edited is left alone. The replaced policy goes to the history and the
-- revision moves on; running again changes nothing.
DO $$
DECLARE
    p     jsonb;
    upd   jsonb;
    hist  jsonb;
    rules text;
BEGIN
    SELECT value INTO p FROM settings WHERE key = 'failure_policy' FOR UPDATE;
    IF p IS NULL THEN
        RETURN;
    END IF;
    upd := p;
    FOREACH rules IN ARRAY ARRAY['account_rules', 'retry_rules'] LOOP
        CONTINUE WHEN jsonb_typeof(p->rules) IS DISTINCT FROM 'array';
        upd := upd || jsonb_build_object(rules, (
            SELECT coalesce(jsonb_agg(
                CASE WHEN c.keywords IS NULL THEN e
                     ELSE jsonb_set(e, '{match,keywords}', c.keywords)
                          || CASE WHEN e->>'name' = c.old_name THEN jsonb_build_object('name', c.name) ELSE '{}' END
                END ORDER BY i), '[]'::jsonb)
            FROM jsonb_array_elements(p->rules) WITH ORDINALITY AS t(e, i)
            LEFT JOIN (VALUES
                ('account_rules', 'login-invalid',
                 '["oauth access token has been revoked", "oauth token has been revoked", "oauth refresh token is no longer valid",
                   "oauth_refresh_dead", "not_logged_in", "has cleared its stored login", "invalid x-api-key", "invalid bearer token",
                   "the security token included in the request is invalid", "your account is not authorized"]'::jsonb,
                 '["stored_login_refused_restart_needed"]'::jsonb,
                 '["oauth access token has been revoked", "oauth token has been revoked", "invalid x-api-key", "invalid bearer token",
                   "the security token included in the request is invalid", "your account is not authorized"]'::jsonb,
                 NULL, NULL),
                ('account_rules', 'account-on-hold',
                 '["oauth_account_on_hold", "oauth authentication is currently not allowed for this organization"]',
                 '["stored_login_refused_admin"]',
                 '["oauth authentication is currently not allowed for this organization"]',
                 NULL, NULL),
                ('retry_rules', 'rate-limit',
                 '["exceed your account''s rate limit", "gateway concurrency limit reached"]',
                 '[]',
                 '["exceed your account''s rate limit"]',
                 'Account quota and gateway concurrency (429)', 'Account quota (429)')
            ) AS c(kind, id, written, appended, keywords, old_name, name)
                ON c.kind = rules AND e->>'id' = c.id
               AND e->'match'->'keywords' IN (c.written, c.written || c.appended)));
    END LOOP;

    IF upd = p THEN
        RETURN;
    END IF;

    SELECT value INTO hist FROM settings WHERE key = 'failure_policy_history' FOR UPDATE;
    hist := jsonb_build_array(p) || coalesce(hist->'policies', '[]'::jsonb);
    IF jsonb_array_length(hist) > 10 THEN
        hist := (SELECT jsonb_agg(e ORDER BY i) FROM jsonb_array_elements(hist) WITH ORDINALITY AS t(e, i) WHERE i <= 10);
    END IF;
    INSERT INTO settings (key, value, updated_at) VALUES ('failure_policy_history', jsonb_build_object('policies', hist), now())
    ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

    UPDATE settings SET value = upd || jsonb_build_object('revision', coalesce((p->>'revision')::bigint, 0) + 1),
            updated_by = NULL, updated_at = now()
    WHERE key = 'failure_policy';
END $$;

-- Kept only for the cores before 0.1.290 (CONTRACTS §101.4, §101.6). The scheduling
-- settings' mode is no longer read or written; a row of the former "default"
-- mode (or any but "traffic") with the limit load off had the metric and
-- model limits greyed out, not chosen, so they take the defaults (§89).
UPDATE settings
SET value = CASE WHEN value->>'mode' <> 'traffic' AND NOT coalesce((value->>'limit_load')::boolean, false)
                 THEN value || '{"metric": "sessions", "model_limits": false}'
                 ELSE value END - 'mode'
WHERE key = 'scheduling' AND value ? 'mode';

-- The mark of the bare upstream ids imported before the task framework.
ALTER TABLE async_tasks DROP COLUMN IF EXISTS legacy_imported;

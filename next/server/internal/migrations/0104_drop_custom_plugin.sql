-- The built-in plugin custom (custom upstream: an account type for each of
-- the openai, anthropic and gemini platforms) is removed (core 0.1.300,
-- CONTRACTS §106): an API key account of the built-in openai, anthropic or
-- gemini plugin takes any compatible Base URL and the platform converts
-- protocols, so accounts no longer override their upstream (the account
-- upstream layer went with it). Its last source is commit 845d9aa47.
-- This uninstalls the plugin the way plugin/install.Uninstall does with
-- purge (without the node barrier: every node is stopped while a release
-- migrates, primary first). custom was a runtime "none" package that never
-- declared user permissions, a database permission or jobs: there are no
-- permissions or grants (the authorization policy does not change), no
-- schema, role or plugin migrations and no tasks to remove.
--
-- Kept on purpose:
--   - accounts with plugin_key 'custom' (and their groups, usage rows,
--     ledger entries): they show as orphaned, like the accounts of any
--     uninstalled plugin; an administrator deletes them in the console
--     (CHANGELOG 0.1.300 asks to do it before the upgrade);
--   - usage_logs, balance_ledger and audit_logs rows: billing history;
--   - plugin_history rows: the plugin's operating history, kept by
--     install.Uninstall too.
--
-- One way: the way back is the pre-upgrade backup. Safe to run again.

SET LOCAL lock_timeout = '10s';

-- The key stays with its publisher (install.retireKey); the removed keys
-- (install.removedKeys) refuse any new install of it.
INSERT INTO plugin_key_retirements (plugin_key, publisher_id)
SELECT key, publisher_id FROM plugins WHERE key = 'custom'
ON CONFLICT (plugin_key) DO UPDATE SET publisher_id = EXCLUDED.publisher_id, retired_by = NULL, retired_at = now();

-- Cascades the versions (with their staged grants), the host permission
-- grants, rollouts, runtime nodes and the uninstall barrier.
DELETE FROM plugins WHERE key = 'custom';
DELETE FROM plugin_egress_domains WHERE plugin_key = 'custom';

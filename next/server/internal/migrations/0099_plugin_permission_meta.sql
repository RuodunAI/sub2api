-- Plugin permission metadata (docs/plugin-permissions ch.2): risk levels
-- replace the sensitive flag, plugin permission groups, soft dependencies,
-- scopes and the manifest's suggested grants, kept per permission. A plugin
-- permission no longer declared becomes status 'removed' (its grants stay and
-- grant nothing) instead of being deleted. Reversed by
-- deploy/rollback/0.1.289-plugin-perm-down.sql.
--
-- permissions.sensitive stays for one release so the previous core still
-- runs on this schema: new code writes it as risk <> 'low' and never reads
-- it. The next migration drops it.
--
-- Safe to run again: every change is IF NOT EXISTS or converges, and the
-- risk backfill only touches rows still at the default.

SET LOCAL lock_timeout = '10s';

-- low | sensitive | critical. Core: superOnly -> critical, sensitive ->
-- sensitive (the core sync writes it); plugins: manifest risk.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS risk varchar(10) NOT NULL DEFAULT 'low';
-- Soft dependencies (qualified keys), editor hints only, core and plugin alike.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS requires text[] NOT NULL DEFAULT '{}';
-- Plugin permission group: key, label and order, copied onto each permission
-- of the group (the manifest's permissionGroups[]); '' for core.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_key varchar(50) NOT NULL DEFAULT '';
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_label jsonb NOT NULL DEFAULT '{}';
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS group_sort integer NOT NULL DEFAULT 0;
-- A plugin permission's scope as the manifest declares it (manifest
-- PermissionScope, resource set to its ResourceName(), so "value" for an
-- enum): {"resource", "own", "options", "values": [{"key", "label"}]}.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS scope jsonb NOT NULL DEFAULT '{}';
-- The manifest's suggested grants: {"<role>": ["", "own", "flow/3"]},
-- canonical suffixes, "" for every row. Shown by GET /permissions (the
-- console's "restore plugin suggestion"); never granted by itself.
ALTER TABLE permissions ADD COLUMN IF NOT EXISTS defaults jsonb NOT NULL DEFAULT '{}';
-- "plugin:" + a resource of up to 30 characters. Widening a varchar only
-- changes the catalog, so a re-run rewrites nothing.
ALTER TABLE permissions ALTER COLUMN scope_type TYPE varchar(40);

DO $$ BEGIN
    ALTER TABLE permissions ADD CONSTRAINT permissions_risk_check CHECK (risk IN ('low', 'sensitive', 'critical'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Existing rows keep what sensitive said until their next sync.
UPDATE permissions SET risk = 'sensitive' WHERE sensitive AND risk = 'low';

COMMENT ON COLUMN permissions.status IS
    'active | disabled (owning plugin disabled) | removed (dropped from code or no longer declared by its plugin; grants kept, granting nothing)';

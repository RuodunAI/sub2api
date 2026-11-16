-- Casbin policy (CONTRACTS §99): casbin_rule, in the gorm-adapter column
-- layout, becomes the one store of who holds which grant. The role tables
-- role_permissions, role_permission_scopes and user_roles, and the
-- superuser flag's grant, are turned into its rows and the three tables
-- dropped (with permissions.sensitive, which 0099's risk replaced):
--
--   p, r:<role key>, <permission key>, *          role_permissions
--   p, r:<role key>, <permission key>, <scope>    role_permission_scopes
--   p, r:<role key>, *, *                         roles.superuser
--   g, u:<user id>, r:<role key>, ''              user_roles
--
-- Grants of removed, disabled or staged permissions are kept (CONTRACTS
-- §21): the loader filters on permissions.status. Soft-deleted users keep
-- their rows, as user_roles did; the loader filters on users.deleted_at and
-- status. roles, permissions, role_assignable_roles and authz_meta stay
-- the metadata. The core writes casbin_rule in its business transactions
-- (internal/authz/casbinrule), including what the foreign keys of the
-- dropped tables cascaded: a deleted role's, permission's or plugin's rows.
--
-- One way, and a release that stops every node first (primary first): a
-- core of an earlier version cannot run on this schema. The way back is a
-- restore of the backup taken before it (deploy/rollback/0.1.290-casbin-down.md).
--
-- Safe to run again: the table, indexes and constraints are IF NOT EXISTS
-- or skip an existing one, and the conversion runs while the role tables
-- exist only, dropping them in the same transaction.

SET LOCAL lock_timeout = '10s';

-- The authz lock first, as every authz write takes it.
SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

-- Keys whose rows would read as reserved policy values stop the upgrade,
-- saying which: a role keyed _self has the subject of the virtual role
-- every active user holds (casbinx.SelfRole), so its grants would reach
-- everyone; a permission keyed * is the action of a superuser row, so its
-- grants would make superusers. The one superuser role is super_admin:
-- the one-holder index below names it. The API writes none of these. Nor
-- does it write a second holder of the role, which that index refuses:
-- 0097 asked for one and its trigger kept it so, but a repair that bypassed
-- the trigger may have left several (a deleted holder counts, as its row
-- is converted too); the server CLI of the release before (0.1.288) keeps
-- one on this schema.
DO $$
DECLARE
    bad text;
    holders bigint[];
BEGIN
    IF EXISTS (SELECT 1 FROM roles WHERE key = '_self') THEN
        RAISE EXCEPTION 'roles.key _self is reserved: its Casbin subject is the virtual self role'
            USING ERRCODE = 'check_violation', HINT = 'rename the role (UPDATE roles SET key = ''<another key>'' WHERE key = ''_self'') and start again';
    END IF;
    IF EXISTS (SELECT 1 FROM permissions WHERE key = '*') THEN
        RAISE EXCEPTION 'permissions.key * is reserved: it is the Casbin action of a superuser role'
            USING ERRCODE = 'check_violation', HINT = 'delete the permission (DELETE FROM permissions WHERE key = ''*'') and start again';
    END IF;
    SELECT string_agg(key, ', ' ORDER BY key) INTO bad FROM roles WHERE superuser AND key <> 'super_admin';
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'only the role super_admin may be a superuser role, not %', bad
            USING ERRCODE = 'check_violation', HINT = 'clear the flag (UPDATE roles SET superuser = false WHERE key <> ''super_admin'') and start again';
    END IF;
    IF to_regclass('public.user_roles') IS NOT NULL THEN
        SELECT array_agg(DISTINCT ur.user_id ORDER BY ur.user_id) INTO holders
        FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE r.superuser;
        IF cardinality(holders) > 1 THEN
            RAISE EXCEPTION 'several super admins: user ids %', array_to_string(holders, ', ')
                USING ERRCODE = 'check_violation', HINT = 'keep one with the 0.1.288 binary: "sub2api admin recover-superadmin --email <the email of the one to keep>" (the others become admins), then start again';
        END IF;
    END IF;
END $$;

DO $$ BEGIN
    ALTER TABLE roles ADD CONSTRAINT roles_key_reserved CHECK (key <> '_self');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE roles ADD CONSTRAINT roles_superuser_key CHECK (NOT superuser OR key = 'super_admin');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE permissions ADD CONSTRAINT permissions_key_reserved CHECK (key <> '*');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- gorm-adapter's columns and unique index (same name and columns), each
-- column as wide as what it holds: v0 a subject (r: and roles.key
-- varchar(50), or u: and a bigint), v1 a permission key (varchar(150)),
-- '*' or a role subject, v2 a scope (varchar(200)), '*' or ''; v3..v5 are
-- unused. A wider roles.key, permissions.key or scope widens these too.
-- casbinx loads with the adapter's AutoMigrate off, which would change them.
-- The shape check keeps every row one the core reads as written: a g row's
-- subject parses as a user id, no row names the virtual self role, no
-- column the loader needs is empty, and only super_admin (the one
-- superuser role, roles_superuser_key) has the superuser row (action *).
CREATE TABLE IF NOT EXISTS casbin_rule (
    id    bigserial    PRIMARY KEY,
    ptype varchar(1)   NOT NULL DEFAULT '',
    v0    varchar(52)  NOT NULL DEFAULT '',
    v1    varchar(150) NOT NULL DEFAULT '',
    v2    varchar(200) NOT NULL DEFAULT '',
    v3    varchar(1)   NOT NULL DEFAULT '',
    v4    varchar(1)   NOT NULL DEFAULT '',
    v5    varchar(1)   NOT NULL DEFAULT '',
    CONSTRAINT casbin_rule_shape CHECK (v3 = '' AND v4 = '' AND v5 = '' AND CASE ptype
        WHEN 'p' THEN v0 ~ '^r:.' AND v0 <> 'r:_self' AND v1 <> '' AND v2 <> '' AND (v1 <> '*' OR (v2 = '*' AND v0 = 'r:super_admin'))
        WHEN 'g' THEN v0 ~ '^u:[0-9]+$' AND v1 ~ '^r:.' AND v1 <> 'r:_self' AND v2 = ''
        ELSE false END)
);
COMMENT ON TABLE casbin_rule IS
    'Casbin policy (gorm-adapter layout): p = a role''s grants (scope * every row; action * a superuser role), g = a user''s roles. Written by internal/authz/casbinrule only';
CREATE UNIQUE INDEX IF NOT EXISTS idx_casbin_rule ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5);
-- The p rows of a permission (its delete, its holders) and the g rows of a
-- role (its delete, its holders); the unique index serves a role's p rows
-- and a user's g rows.
CREATE INDEX IF NOT EXISTS casbin_rule_v1_idx ON casbin_rule (ptype, v1);
-- One super admin (0097's user_roles_one_superuser, moved): at most one g
-- row of the superuser role. Not deferrable, so a transfer revokes before it
-- grants; a second holder written concurrently waits for the first to
-- commit and then fails with unique_violation.
CREATE UNIQUE INDEX IF NOT EXISTS casbin_rule_one_superuser ON casbin_rule (v1) WHERE ptype = 'g' AND v1 = 'r:super_admin';

-- Two counters beside authz_meta.version, which every authz write moves
-- (the X-Authz-Version of the console, the user caches):
--   - policy_version moves with the writes of what the policy snapshot
--     holds (grants, permission statuses, role definitions), not with a
--     user's roles or status: a node rebuilds its snapshot on the first,
--     and drops one user's cached roles on the second;
--   - roles.revision moves with every change to the role (its grants,
--     assignable list, name, description, a reset): a role editor sends it
--     back and is refused (version_conflict) when the role changed since.
ALTER TABLE authz_meta ADD COLUMN IF NOT EXISTS policy_version bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN authz_meta.policy_version IS
    'moved with version by the writes of grants, permission statuses and role definitions only (CONTRACTS §99.4)';
ALTER TABLE roles ADD COLUMN IF NOT EXISTS revision bigint NOT NULL DEFAULT 0;
COMMENT ON COLUMN roles.revision IS
    'moved by every change to the role; its editors send it back (optimistic concurrency, CONTRACTS §99.4)';

-- The conversion, while the role tables exist: their rows as policy rows
-- (UNION drops a grant held both ways), then the tables go, their
-- triggers (0097's user_roles_one_superuser) with them. Both versions
-- move, so that no node answers from a role cache or a policy snapshot of
-- the old tables.
DO $$
BEGIN
    IF to_regclass('public.user_roles') IS NULL THEN
        RETURN;
    END IF;
    INSERT INTO casbin_rule (ptype, v0, v1, v2)
    SELECT 'p', 'r:' || r.key, p.key, '*'
    FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id
    UNION
    SELECT 'p', 'r:' || r.key, p.key, rs.scope
    FROM role_permission_scopes rs JOIN roles r ON r.id = rs.role_id JOIN permissions p ON p.id = rs.permission_id
    UNION
    SELECT 'p', 'r:' || r.key, '*', '*' FROM roles r WHERE r.superuser
    UNION
    SELECT 'g', 'u:' || ur.user_id, 'r:' || r.key, ''
    FROM user_roles ur JOIN roles r ON r.id = ur.role_id;
    DROP TABLE user_roles, role_permission_scopes, role_permissions;
    DROP FUNCTION IF EXISTS user_roles_one_superuser();
    UPDATE authz_meta SET version = version + 1, policy_version = policy_version + 1 WHERE id = 1;
END $$;
ALTER TABLE permissions DROP COLUMN IF EXISTS sensitive;

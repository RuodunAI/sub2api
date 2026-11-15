-- Casbin policy projection (CONTRACTS §99, Mode P): casbin_rule in the
-- gorm-adapter column layout, kept by triggers in the transaction of every
-- write to the role tables, which stay the source of truth. Go code of any
-- version, migrations, FK cascades and manual SQL alike are projected, so
-- nodes of the previous core keep writing during a rolling upgrade and the
-- Go write paths do not change. internal/authz/casbinx only reads it.
-- Reversed by deploy/rollback/0.1.290-casbin-down.sql.
--
--   p, r:<role key>, <permission key>, *          role_permissions
--   p, r:<role key>, <permission key>, <scope>    role_permission_scopes
--   p, r:<role key>, *, *                         roles.superuser
--   g, u:<user id>, r:<role key>, ''              user_roles
--
-- Grants of removed, disabled or staged permissions are projected as well
-- (their grants are kept, CONTRACTS §21): the loader filters on
-- permissions.status. Soft-deleted users keep their rows, as user_roles
-- does; the loader filters on users.deleted_at and status.
--
-- Locks: the authz lock first, then the trigger tables parents first
-- (permissions, roles, then the grant and assignment tables), the order of
-- every writer: the authz writers take the authz lock before any role
-- table, an FK cascade (a plugin's or a role's delete) runs from the parent
-- down. users is not touched at all: iam's createUserTx inserts its row
-- before it takes the authz lock, and logins update it.
--
-- Cost: the backfill reads every grant and assignment while holding these
-- locks, so it grows linearly with user_roles (about 2.5 µs a row, 0.5 s for
-- 200,000 assignments); a row trigger adds about 20 µs to each row written.
--
-- Safe to run again: everything is IF NOT EXISTS or OR REPLACE, and the
-- backfill is authz_casbin_repair(), which converges (and moves the version
-- only when it changed something).

SET LOCAL lock_timeout = '10s';

-- The authz lock first, as every authz write takes it: no grant commits
-- between the triggers' creation and the backfill.
SELECT 1 FROM authz_meta WHERE id = 1 FOR UPDATE;

-- gorm-adapter's layout and unique index (same name and columns), but NOT
-- NULL DEFAULT '' and v1/v2 as wide as what they hold: permissions.key is
-- varchar(150) (a role subject is at most 52), a scope varchar(200); the
-- adapter's varchar(100) holds neither. casbinx loads with the adapter's
-- AutoMigrate off, which would narrow them back.
CREATE TABLE IF NOT EXISTS casbin_rule (
    id    bigserial    PRIMARY KEY,
    ptype varchar(100) NOT NULL DEFAULT '',
    v0    varchar(100) NOT NULL DEFAULT '',
    v1    varchar(150) NOT NULL DEFAULT '',
    v2    varchar(200) NOT NULL DEFAULT '',
    v3    varchar(100) NOT NULL DEFAULT '',
    v4    varchar(100) NOT NULL DEFAULT '',
    v5    varchar(100) NOT NULL DEFAULT ''
);
COMMENT ON TABLE casbin_rule IS
    'Casbin policy (gorm-adapter layout): the projection of the role tables, kept by the migration 0100 triggers; the core only reads it';
CREATE UNIQUE INDEX IF NOT EXISTS idx_casbin_rule ON casbin_rule (ptype, v0, v1, v2, v3, v4, v5);
-- The p rows of a permission (its delete), the g rows of a role (its delete
-- or rename); the unique index serves a role's p rows and a user's g rows.
CREATE INDEX IF NOT EXISTS casbin_rule_v1_idx ON casbin_rule (ptype, v1);

-- The mapping's values, the constants of internal/authz/casbinx/api.go:
-- UserSub, RoleSub, SuperKey and AllRows. The view and every trigger build
-- rows with these alone. Plain SQL without a search_path of their own, so
-- that they inline (an index still serves v0 = authz_casbin_role_sub($1));
-- their bodies name nothing outside pg_catalog, which is searched first.
CREATE OR REPLACE FUNCTION authz_casbin_user_sub(user_id bigint) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT 'u:' || user_id::text $$;
CREATE OR REPLACE FUNCTION authz_casbin_role_sub(role_key text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT 'r:' || role_key $$;
-- The action of a superuser role's row: it grants every key.
CREATE OR REPLACE FUNCTION authz_casbin_super_key() RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT '*'::text $$;
-- The scope of an unscoped grant (every row).
CREATE OR REPLACE FUNCTION authz_casbin_all_rows() RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT '*'::text $$;

-- The mapping: the backfill, the drift check and authz_casbin_repair read
-- it, the triggers write the same rows with the functions above.
CREATE OR REPLACE VIEW authz_casbin_expected AS
    SELECT 'p'::text AS ptype, authz_casbin_role_sub(r.key) AS v0, p.key::text AS v1, authz_casbin_all_rows() AS v2, ''::text AS v3, ''::text AS v4, ''::text AS v5
    FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id
    UNION ALL
    SELECT 'p', authz_casbin_role_sub(r.key), p.key, rs.scope, '', '', ''
    FROM role_permission_scopes rs JOIN roles r ON r.id = rs.role_id JOIN permissions p ON p.id = rs.permission_id
    UNION ALL
    SELECT 'p', authz_casbin_role_sub(r.key), authz_casbin_super_key(), authz_casbin_all_rows(), '', '', '' FROM roles r WHERE r.superuser
    UNION ALL
    SELECT 'g', authz_casbin_user_sub(ur.user_id), authz_casbin_role_sub(r.key), '', '', '', ''
    FROM user_roles ur JOIN roles r ON r.id = ur.role_id;

-- Rows that differ, either way: kind 'missing' is expected but not in
-- casbin_rule, 'extra' is in casbin_rule but not expected. Empty when the
-- projection is exact. Both sides in full: about 0.8 s for 200,000 rows.
CREATE OR REPLACE VIEW authz_casbin_drift AS
    SELECT 'missing'::text AS kind, m.* FROM (
        SELECT ptype, v0, v1, v2, v3, v4, v5 FROM authz_casbin_expected
        EXCEPT
        SELECT ptype, v0, v1, v2, v3, v4, v5 FROM casbin_rule) m
    UNION ALL
    SELECT 'extra'::text, x.* FROM (
        SELECT ptype, v0, v1, v2, v3, v4, v5 FROM casbin_rule
        EXCEPT
        SELECT ptype, v0, v1, v2, v3, v4, v5 FROM authz_casbin_expected) x;

-- Every function below runs with search_path public: a session whose
-- search_path is a plugin schema (plugin connections and migrations) still
-- writes the core tables through them, and must neither miss them nor read
-- a plugin's table of the same name.

-- authz_casbin_repair makes casbin_rule the expected rows again (extra ones
-- deleted, missing ones inserted) and returns how many rows it changed.
-- The backfill below, the drift task and the TRUNCATE triggers run it.
-- Callers outside a trigger take the authz lock first (Bump), so that no
-- grant commits halfway.
CREATE OR REPLACE FUNCTION authz_casbin_repair() RETURNS bigint LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
DECLARE
    removed bigint;
    added   bigint;
BEGIN
    DELETE FROM casbin_rule c USING authz_casbin_drift d
    WHERE d.kind = 'extra'
      AND (c.ptype, c.v0, c.v1, c.v2, c.v3, c.v4, c.v5) = (d.ptype, d.v0, d.v1, d.v2, d.v3, d.v4, d.v5);
    GET DIAGNOSTICS removed = ROW_COUNT;
    INSERT INTO casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
    SELECT ptype, v0, v1, v2, v3, v4, v5 FROM authz_casbin_drift WHERE kind = 'missing'
    ON CONFLICT DO NOTHING;
    GET DIAGNOSTICS added = ROW_COUNT;
    RETURN removed + added;
END $$;

-- Keys whose rows would read as reserved policy values are refused: a role
-- key whose subject is casbinx.SelfRole (r:_self, the virtual role every
-- active user holds) would hand its grants to every user, a permission key
-- of SuperKey would make its grants superuser rows. The API never writes
-- either (roleKeyRe, the catalog's key syntax); this covers SQL.
CREATE OR REPLACE FUNCTION authz_casbin_check_role_key(role_key text) RETURNS void LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF authz_casbin_role_sub(role_key) = 'r:_self' THEN
        RAISE EXCEPTION 'roles.key % is reserved: its Casbin subject is the virtual self role', role_key
            USING ERRCODE = 'check_violation', HINT = 'rename the role (UPDATE roles SET key = ''<another key>'' WHERE id = <its id>)';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION authz_casbin_check_permission_key(permission_key text) RETURNS void LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF permission_key = authz_casbin_super_key() THEN
        RAISE EXCEPTION 'permissions.key % is reserved: it is the Casbin action of a superuser role', permission_key
            USING ERRCODE = 'check_violation', HINT = 'delete the permission (DELETE FROM permissions WHERE key = ''*'')';
    END IF;
END $$;

-- authz_casbin_set makes the row (pt, a, b, c) present or absent.
CREATE OR REPLACE FUNCTION authz_casbin_set(pt text, a text, b text, c text, present boolean) RETURNS void LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF present THEN
        INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES (pt, a, b, c) ON CONFLICT DO NOTHING;
    ELSE
        DELETE FROM casbin_rule WHERE ptype = pt AND v0 = a AND v1 = b AND v2 = c AND v3 = '' AND v4 = '' AND v5 = '';
    END IF;
END $$;

-- Row triggers set the one policy row a changed row maps to from what the
-- tables hold when the trigger fires (after the statement) instead of
-- replaying the operation, because two sources map to one row: an unscoped
-- grant is both a role_permissions row and a role_permission_scopes row of
-- scope '*'. Deleting one of them must keep the row while the other holds
-- it, in whatever order a statement's events come (a data-modifying CTE
-- runs after its main statement). A row whose role or permission is gone
-- was deleted by an FK cascade: the parent's trigger clears its policy rows.

-- authz_casbin_sync_grant reconciles the p row of role role_id_ holding
-- permission permission_id_ with scope_.
CREATE OR REPLACE FUNCTION authz_casbin_sync_grant(role_id_ bigint, permission_id_ bigint, scope_ text) RETURNS void LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
DECLARE
    role text;
    pkey text;
BEGIN
    SELECT authz_casbin_role_sub(key) INTO role FROM roles WHERE id = role_id_;
    SELECT key INTO pkey FROM permissions WHERE id = permission_id_;
    IF role IS NULL OR pkey IS NULL THEN
        RETURN;
    END IF;
    PERFORM authz_casbin_set('p', role, pkey, scope_,
        EXISTS (SELECT 1 FROM role_permission_scopes WHERE role_id = role_id_ AND permission_id = permission_id_ AND scope = scope_)
        OR (scope_ = authz_casbin_all_rows() AND EXISTS (SELECT 1 FROM role_permissions WHERE role_id = role_id_ AND permission_id = permission_id_)));
END $$;

-- The trigger of both grant tables: TG_ARGV[0] is 'scoped' on
-- role_permission_scopes, whose rows carry a scope; a role_permissions row
-- grants every row.
CREATE OR REPLACE FUNCTION authz_casbin_on_grant() RETURNS trigger LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
DECLARE
    old_scope text := authz_casbin_all_rows();
    new_scope text := authz_casbin_all_rows();
BEGIN
    IF TG_ARGV[0] = 'scoped' THEN
        IF TG_OP <> 'INSERT' THEN
            old_scope := OLD.scope;
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_scope := NEW.scope;
        END IF;
    END IF;
    IF TG_OP <> 'INSERT' THEN
        PERFORM authz_casbin_sync_grant(OLD.role_id, OLD.permission_id, old_scope);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM authz_casbin_sync_grant(NEW.role_id, NEW.permission_id, new_scope);
    END IF;
    RETURN NULL;
END $$;

-- authz_casbin_sync_user_role reconciles the g row of user user_id_ holding
-- role role_id_. A hard-deleted user's rows go here too: user_roles
-- cascades from users, and TRUNCATE users needs CASCADE, which truncates
-- user_roles (its statement trigger rebuilds), so users has no trigger.
CREATE OR REPLACE FUNCTION authz_casbin_sync_user_role(user_id_ bigint, role_id_ bigint) RETURNS void LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
DECLARE
    role text;
BEGIN
    SELECT authz_casbin_role_sub(key) INTO role FROM roles WHERE id = role_id_;
    IF role IS NULL THEN
        RETURN;
    END IF;
    PERFORM authz_casbin_set('g', authz_casbin_user_sub(user_id_), role, '',
        EXISTS (SELECT 1 FROM user_roles WHERE user_id = user_id_ AND role_id = role_id_));
END $$;

CREATE OR REPLACE FUNCTION authz_casbin_on_user_role() RETURNS trigger LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        PERFORM authz_casbin_sync_user_role(OLD.user_id, OLD.role_id);
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM authz_casbin_sync_user_role(NEW.user_id, NEW.role_id);
    END IF;
    RETURN NULL;
END $$;

-- Roles: the superuser row; a deleted role's rows (its grants and holders
-- cascade, their triggers skip); a renamed role's rows. Keys do not change
-- through the API (0.1.287), but SyncCore's start-up error asks the operator
-- to rename a custom role holding a new built-in key with UPDATE roles SET
-- key, so a rename moves the role's rows, read again through the mapping.
CREATE OR REPLACE FUNCTION authz_casbin_on_role() RETURNS trigger LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP <> 'DELETE' AND (TG_OP = 'INSERT' OR NEW.key IS DISTINCT FROM OLD.key) THEN
        PERFORM authz_casbin_check_role_key(NEW.key);
    END IF;
    IF TG_OP <> 'INSERT' AND (TG_OP = 'DELETE' OR NEW.key IS DISTINCT FROM OLD.key) THEN
        DELETE FROM casbin_rule
        WHERE (ptype = 'p' AND v0 = authz_casbin_role_sub(OLD.key)) OR (ptype = 'g' AND v1 = authz_casbin_role_sub(OLD.key));
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.key IS DISTINCT FROM OLD.key THEN
        INSERT INTO casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
        SELECT * FROM authz_casbin_expected
        WHERE (ptype = 'p' AND v0 = authz_casbin_role_sub(NEW.key)) OR (ptype = 'g' AND v1 = authz_casbin_role_sub(NEW.key))
        ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    PERFORM authz_casbin_set('p', authz_casbin_role_sub(NEW.key), authz_casbin_super_key(), authz_casbin_all_rows(),
        EXISTS (SELECT 1 FROM roles WHERE id = NEW.id AND superuser));
    RETURN NULL;
END $$;

-- Permissions: a deleted one's grants (they cascade, their triggers skip).
-- A key is the permission's identity everywhere (catalog, access keys,
-- plugin manifests) and nothing renames one, so a rename is refused rather
-- than projected (CONTRACTS §99.1 says how a migration renames one). An
-- update writing the same key (an upsert's SET key = EXCLUDED.key) is not a
-- rename.
CREATE OR REPLACE FUNCTION authz_casbin_on_permission() RETURNS trigger LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM authz_casbin_check_permission_key(NEW.key);
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF NEW.key IS DISTINCT FROM OLD.key THEN
            RAISE EXCEPTION 'permissions.key is immutable: % cannot become %', OLD.key, NEW.key
                USING HINT = 'casbin_rule (migration 0100) projects grants by permission key; see CONTRACTS §99.1 for renaming one in a migration';
        END IF;
        RETURN NULL;
    END IF;
    DELETE FROM casbin_rule WHERE ptype = 'p' AND v1 = OLD.key;
    RETURN NULL;
END $$;

-- TRUNCATE fires no row trigger. Refusing it would break TRUNCATE ...
-- CASCADE of a parent (plugins, users) and resets of test or staging
-- databases; instead the projection is rebuilt from the mapping in the same
-- transaction, which keeps it exact whatever was truncated, casbin_rule
-- itself included.
CREATE OR REPLACE FUNCTION authz_casbin_on_truncate() RETURNS trigger LANGUAGE plpgsql
SET search_path = public, pg_temp AS $$
BEGIN
    PERFORM authz_casbin_repair();
    RETURN NULL;
END $$;

-- Parents first (see Locks above). CREATE OR REPLACE TRIGGER takes the
-- same SHARE ROW EXCLUSIVE lock on a re-run as on the first; DROP TRIGGER
-- would take ACCESS EXCLUSIVE and block every reader until commit.
CREATE OR REPLACE TRIGGER casbin_sync AFTER INSERT OR UPDATE OF key OR DELETE ON permissions
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_permission();
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON permissions
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
CREATE OR REPLACE TRIGGER casbin_sync AFTER INSERT OR UPDATE OF key, superuser OR DELETE ON roles
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_role();
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON roles
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
CREATE OR REPLACE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON role_permissions
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_grant('unscoped');
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON role_permissions
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
CREATE OR REPLACE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON role_permission_scopes
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_grant('scoped');
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON role_permission_scopes
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
CREATE OR REPLACE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON user_roles
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_user_role();
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON user_roles
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
CREATE OR REPLACE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON casbin_rule
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();

-- 0097's trigger function on user_roles, under the same search_path (the
-- rollback script resets it).
ALTER FUNCTION user_roles_one_superuser() SET search_path = public, pg_temp;

-- Keys written before the triggers existed: a reserved one stops the
-- upgrade, with the error saying which.
DO $$
BEGIN
    PERFORM authz_casbin_check_role_key(key) FROM roles;
    PERFORM authz_casbin_check_permission_key(key) FROM permissions;
END $$;

-- Backfill (a re-run converges instead); the version moves when it wrote
-- something, so that the nodes read the projection anew.
DO $$
BEGIN
    IF authz_casbin_repair() > 0 THEN
        UPDATE authz_meta SET version = version + 1 WHERE id = 1;
    END IF;
END $$;

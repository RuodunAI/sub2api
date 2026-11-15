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
-- Safe to run again: everything is IF NOT EXISTS or OR REPLACE, triggers are
-- dropped and created, and the backfill is authz_casbin_repair(), which
-- converges (and moves the version only when it changed something).

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
-- The scoped grants naming a row (ClearScopeGrants' patterns), for reading
-- the policy by scope.
CREATE INDEX IF NOT EXISTS casbin_rule_v2_idx ON casbin_rule (v2) WHERE ptype = 'p';

-- The mapping, in this one place: the backfill, the drift check and
-- authz_casbin_repair read it; the triggers write the same rows.
CREATE OR REPLACE VIEW authz_casbin_expected AS
    SELECT 'p'::text AS ptype, 'r:' || r.key AS v0, p.key::text AS v1, '*'::text AS v2, ''::text AS v3, ''::text AS v4, ''::text AS v5
    FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id
    UNION ALL
    SELECT 'p', 'r:' || r.key, p.key, rs.scope, '', '', ''
    FROM role_permission_scopes rs JOIN roles r ON r.id = rs.role_id JOIN permissions p ON p.id = rs.permission_id
    UNION ALL
    SELECT 'p', 'r:' || r.key, '*', '*', '', '', '' FROM roles r WHERE r.superuser
    UNION ALL
    SELECT 'g', 'u:' || ur.user_id, 'r:' || r.key, '', '', '', ''
    FROM user_roles ur JOIN roles r ON r.id = ur.role_id;

-- Rows that differ, either way: kind 'missing' is expected but not in
-- casbin_rule, 'extra' is in casbin_rule but not expected. Empty when the
-- projection is exact.
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

-- authz_casbin_repair makes casbin_rule the expected rows again (extra ones
-- deleted, missing ones inserted) and returns how many rows it changed.
-- The backfill below, the drift task and the TRUNCATE triggers run it.
-- Callers outside a trigger take the authz lock first (Bump), so that no
-- grant commits halfway.
CREATE OR REPLACE FUNCTION authz_casbin_repair() RETURNS bigint LANGUAGE plpgsql AS $$
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

-- authz_casbin_set makes the row (pt, a, b, c) present or absent.
CREATE OR REPLACE FUNCTION authz_casbin_set(pt text, a text, b text, c text, present boolean) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    IF present THEN
        INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES (pt, a, b, c) ON CONFLICT DO NOTHING;
    ELSE
        DELETE FROM casbin_rule WHERE ptype = pt AND v0 = a AND v1 = b AND v2 = c AND v3 = '' AND v4 = '' AND v5 = '';
    END IF;
END $$;

-- Row triggers reconcile the one policy row a changed row maps to with what
-- the tables hold when the trigger fires (after the statement), rather than
-- replaying the operation: a statement may delete and insert the same grant
-- (data-modifying CTEs), and an unscoped grant is reachable from both
-- role_permissions and a role_permission_scopes row of scope '*'. A row
-- whose role or permission is gone was deleted by an FK cascade: the
-- parent's trigger clears its policy rows.

-- authz_casbin_sync_grant reconciles the p row of role roleID holding
-- permission permID with scope.
CREATE OR REPLACE FUNCTION authz_casbin_sync_grant(role_id_ bigint, permission_id_ bigint, scope_ text) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    role text;
    pkey text;
BEGIN
    SELECT 'r:' || key INTO role FROM roles WHERE id = role_id_;
    SELECT key INTO pkey FROM permissions WHERE id = permission_id_;
    IF role IS NULL OR pkey IS NULL THEN
        RETURN;
    END IF;
    PERFORM authz_casbin_set('p', role, pkey, scope_,
        EXISTS (SELECT 1 FROM role_permission_scopes WHERE role_id = role_id_ AND permission_id = permission_id_ AND scope = scope_)
        OR (scope_ = '*' AND EXISTS (SELECT 1 FROM role_permissions WHERE role_id = role_id_ AND permission_id = permission_id_)));
END $$;

CREATE OR REPLACE FUNCTION authz_casbin_on_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- role_permissions has no scope column: its grants are unscoped ('*').
    IF TG_OP <> 'INSERT' THEN
        PERFORM authz_casbin_sync_grant(OLD.role_id, OLD.permission_id, COALESCE(to_jsonb(OLD)->>'scope', '*'));
    END IF;
    IF TG_OP <> 'DELETE' THEN
        PERFORM authz_casbin_sync_grant(NEW.role_id, NEW.permission_id, COALESCE(to_jsonb(NEW)->>'scope', '*'));
    END IF;
    RETURN NULL;
END $$;

-- authz_casbin_sync_user_role reconciles the g row of user userID holding
-- role roleID.
CREATE OR REPLACE FUNCTION authz_casbin_sync_user_role(user_id_ bigint, role_id_ bigint) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    role text;
BEGIN
    SELECT 'r:' || key INTO role FROM roles WHERE id = role_id_;
    IF role IS NULL THEN
        RETURN;
    END IF;
    PERFORM authz_casbin_set('g', 'u:' || user_id_, role, '',
        EXISTS (SELECT 1 FROM user_roles WHERE user_id = user_id_ AND role_id = role_id_));
END $$;

CREATE OR REPLACE FUNCTION authz_casbin_on_user_role() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE OR REPLACE FUNCTION authz_casbin_on_role() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' AND (TG_OP = 'DELETE' OR NEW.key IS DISTINCT FROM OLD.key) THEN
        DELETE FROM casbin_rule WHERE (ptype = 'p' AND v0 = 'r:' || OLD.key) OR (ptype = 'g' AND v1 = 'r:' || OLD.key);
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.key IS DISTINCT FROM OLD.key THEN
        INSERT INTO casbin_rule (ptype, v0, v1, v2, v3, v4, v5)
        SELECT * FROM authz_casbin_expected
        WHERE (ptype = 'p' AND v0 = 'r:' || NEW.key) OR (ptype = 'g' AND v1 = 'r:' || NEW.key)
        ON CONFLICT DO NOTHING;
        RETURN NULL;
    END IF;
    PERFORM authz_casbin_set('p', 'r:' || NEW.key, '*', '*', EXISTS (SELECT 1 FROM roles WHERE id = NEW.id AND superuser));
    RETURN NULL;
END $$;

-- Permissions: a deleted one's grants (they cascade, their triggers skip).
-- A key is the permission's identity everywhere (catalog, access keys,
-- plugin manifests) and nothing renames one, so a rename is refused rather
-- than projected. An update writing the same key (an upsert's SET key =
-- EXCLUDED.key) is not a rename.
CREATE OR REPLACE FUNCTION authz_casbin_on_permission() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.key IS DISTINCT FROM OLD.key THEN
            RAISE EXCEPTION 'permissions.key is immutable: % cannot become %', OLD.key, NEW.key
                USING HINT = 'casbin_rule (migration 0100) projects grants by permission key; delete the permission and create another';
        END IF;
        RETURN NULL;
    END IF;
    DELETE FROM casbin_rule WHERE ptype = 'p' AND v1 = OLD.key;
    RETURN NULL;
END $$;

-- Users: a hard-deleted one's roles (they cascade, their triggers find the
-- role and clear the same rows; this covers a delete with the triggers of
-- user_roles disabled too).
CREATE OR REPLACE FUNCTION authz_casbin_on_user() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM casbin_rule WHERE ptype = 'g' AND v0 = 'u:' || OLD.id;
    RETURN NULL;
END $$;

-- TRUNCATE fires no row trigger. Refusing it would break TRUNCATE ...
-- CASCADE of a parent (plugins, users) and resets of test or staging
-- databases; instead the projection is rebuilt from the mapping in the same
-- transaction, which keeps it exact whatever was truncated, casbin_rule
-- itself included.
CREATE OR REPLACE FUNCTION authz_casbin_on_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM authz_casbin_repair();
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS casbin_sync ON role_permissions;
CREATE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON role_permissions
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_grant();
DROP TRIGGER IF EXISTS casbin_sync ON role_permission_scopes;
CREATE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON role_permission_scopes
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_grant();
DROP TRIGGER IF EXISTS casbin_sync ON user_roles;
CREATE TRIGGER casbin_sync AFTER INSERT OR UPDATE OR DELETE ON user_roles
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_user_role();
DROP TRIGGER IF EXISTS casbin_sync ON roles;
CREATE TRIGGER casbin_sync AFTER INSERT OR UPDATE OF key, superuser OR DELETE ON roles
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_role();
DROP TRIGGER IF EXISTS casbin_sync ON permissions;
CREATE TRIGGER casbin_sync AFTER UPDATE OF key OR DELETE ON permissions
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_permission();
DROP TRIGGER IF EXISTS casbin_sync ON users;
CREATE TRIGGER casbin_sync AFTER DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION authz_casbin_on_user();

DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permissions;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON role_permissions
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON role_permission_scopes;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON role_permission_scopes
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON user_roles;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON user_roles
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON roles;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON roles
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON permissions;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON permissions
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON users;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON users
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();
DROP TRIGGER IF EXISTS casbin_sync_truncate ON casbin_rule;
CREATE TRIGGER casbin_sync_truncate AFTER TRUNCATE ON casbin_rule
    FOR EACH STATEMENT EXECUTE FUNCTION authz_casbin_on_truncate();

-- Backfill (a re-run converges instead); the version moves when it wrote
-- something, so that the nodes read the projection anew.
DO $$
BEGIN
    IF authz_casbin_repair() > 0 THEN
        UPDATE authz_meta SET version = version + 1 WHERE id = 1;
    END IF;
END $$;

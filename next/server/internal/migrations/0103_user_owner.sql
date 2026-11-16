-- Supplier agents and 所属 (CONTRACTS §103, core 0.1.292).
--
-- users.owner_id is the user a user belongs to: a team scope
-- (account:read:team, user:read:team …) reaches the holder's rows and those
-- of every user below it, MaxTeamDepth (8) levels at most. users.owner_path
-- lists a user's owners from the top of its chain down to owner_id (the
-- tree of sdk/hierarchy): the users below A are those whose path contains
-- A, one query on the GIN index. The core writes both together (Move) and
-- checks cycles and depth there; the CHECK only keeps the path's last
-- owner owner_id. A user that owns others is not deleted (no ON DELETE:
-- its id is in their paths); users are only soft-deleted.
--
-- accounts.owner_id is the account's 所属: the user whose account it is for
-- own and team scopes. It starts as the creator (created_by, which never
-- changes) and an administrator may move it (account:owner:update).
--
-- accounts.owner_id is nullable and refers to users: SET NULL only matters
-- to a hard delete by a fixture or a tool. Safe to run again: the paths
-- are computed from owner_id again (the same ones), and the accounts
-- backfill only fills an owner still missing.

SET LOCAL lock_timeout = '10s';

ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_id bigint CONSTRAINT users_owner_fk REFERENCES users(id);
ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_path bigint[] NOT NULL DEFAULT '{}';
WITH RECURSIVE p (id, path) AS (
    SELECT id, '{}'::bigint[] FROM users WHERE owner_id IS NULL
    UNION ALL
    SELECT u.id, p.path || u.owner_id FROM users u JOIN p ON u.owner_id = p.id
)
UPDATE users u SET owner_path = p.path FROM p WHERE u.id = p.id AND u.owner_path IS DISTINCT FROM p.path;
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_owner_not_self CHECK (owner_id <> id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_owner_path_ck CHECK (owner_path[cardinality(owner_path)] IS NOT DISTINCT FROM owner_id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
CREATE INDEX IF NOT EXISTS users_owner_idx ON users (owner_id) WHERE owner_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS users_owner_path_idx ON users USING gin (owner_path);
COMMENT ON COLUMN users.owner_id IS 'the user this user belongs to (team scopes, CONTRACTS §103); NULL for none';
COMMENT ON COLUMN users.owner_path IS 'the owners above this user, the top of its chain first and owner_id last (CONTRACTS §103); written with owner_id';

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS owner_id bigint CONSTRAINT accounts_owner_fk REFERENCES users(id) ON DELETE SET NULL;
UPDATE accounts SET owner_id = created_by WHERE owner_id IS NULL AND created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS accounts_owner_idx ON accounts (owner_id) WHERE deleted_at IS NULL;
COMMENT ON COLUMN accounts.owner_id IS '所属: whose account it is for own and team scopes (CONTRACTS §103); starts as created_by, an administrator may move it';

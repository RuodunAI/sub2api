-- Supplier agents and 所属 (CONTRACTS §103, core 0.1.292).
--
-- users.owner_id is the user a user belongs to: a team scope
-- (account:read:team, user:read:team …) reaches the holder's rows and those
-- of every user below it, MaxTeamDepth (8) levels at most. Cycles and depth
-- are checked by the core under the authz lock, not here.
--
-- accounts.owner_id is the account's 所属: the user whose account it is for
-- own and team scopes. It starts as the creator (created_by, which never
-- changes) and an administrator may move it (account:owner:update).
--
-- Both are nullable and refer to users, which are only soft-deleted: SET
-- NULL only matters to a hard delete by a fixture or a tool. Safe to run
-- again: the backfill only fills an owner still missing.

SET LOCAL lock_timeout = '10s';

ALTER TABLE users ADD COLUMN IF NOT EXISTS owner_id bigint CONSTRAINT users_owner_fk REFERENCES users(id) ON DELETE SET NULL;
DO $$ BEGIN
    ALTER TABLE users ADD CONSTRAINT users_owner_not_self CHECK (owner_id <> id);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
CREATE INDEX IF NOT EXISTS users_owner_idx ON users (owner_id) WHERE owner_id IS NOT NULL;
COMMENT ON COLUMN users.owner_id IS 'the user this user belongs to (team scopes, CONTRACTS §103); NULL for none';

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS owner_id bigint CONSTRAINT accounts_owner_fk REFERENCES users(id) ON DELETE SET NULL;
UPDATE accounts SET owner_id = created_by WHERE owner_id IS NULL AND created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS accounts_owner_idx ON accounts (owner_id) WHERE deleted_at IS NULL;
COMMENT ON COLUMN accounts.owner_id IS '所属: whose account it is for own and team scopes (CONTRACTS §103); starts as created_by, an administrator may move it';

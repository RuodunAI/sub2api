-- Core migration 0100_casbin_rule.sql (core 0.1.290, a candidate version:
-- see 0.1.290-casbin-down.md) has no reverse script: it turns the role
-- tables into casbin_rule and drops them, and every write since went to
-- casbin_rule only. The way back to 0.1.289 is restoring the backup taken
-- before the upgrade. This file stays so that a runbook pointing here stops
-- with that answer and changes nothing:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f 0.1.290-casbin-down.sql

DO $$
BEGIN
    RAISE EXCEPTION 'migration 0100_casbin_rule.sql is one way: restore the backup taken before the 0.1.290 upgrade (see 0.1.290-casbin-down.md)';
END $$;

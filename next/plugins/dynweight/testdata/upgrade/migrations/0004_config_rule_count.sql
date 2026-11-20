-- plg_dynweight 0004 (upgrade test fixture 0.5.13-test, testdata/upgrade):
-- the number of global rules of the saved configuration, backfilled.
-- Expand-only, so 0.5.12 keeps working while both versions run. No model
-- maps the column on purpose: the fixture is packed with --skip-db-check.
-- Runs with search_path pinned to the plugin schema (plg_dynweight).

ALTER TABLE config ADD COLUMN IF NOT EXISTS rule_count integer;

UPDATE config SET rule_count = CASE jsonb_typeof(doc->'rules') WHEN 'array' THEN jsonb_array_length(doc->'rules') ELSE 0 END
WHERE rule_count IS NULL;

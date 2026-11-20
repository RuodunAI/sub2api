-- dynweight 0004 (upgrade test fixture 0.6.1-test, testdata/upgrade):
-- the number of global rules of the saved configuration, backfilled.
-- Expand-only, so 0.6.0 keeps working while both versions run. No model
-- maps the column on purpose: the fixture is packed with --skip-db-check.
-- Runs as the plugin's database role in the core's schema: every table,
-- index and sequence is named plg_dynweight__<name> (CONTRACTS §130).

ALTER TABLE plg_dynweight__config ADD COLUMN IF NOT EXISTS rule_count integer;

UPDATE plg_dynweight__config SET rule_count = CASE jsonb_typeof(doc->'rules') WHEN 'array' THEN jsonb_array_length(doc->'rules') ELSE 0 END
WHERE rule_count IS NULL;

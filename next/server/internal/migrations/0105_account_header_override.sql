-- Request header override of an account (CONTRACTS §108): new-api's channel
-- header_override format, an object of header name → value or placeholder,
-- with "*" and "re:"/"regex:" keys as passthrough rules. NULL means none.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS header_override jsonb;

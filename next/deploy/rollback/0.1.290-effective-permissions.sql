-- The effective permissions of every user, one line per grant, for
-- comparing a database before and after the 0.1.290 upgrade
-- (0.1.290-release.md, "Rehearse on a production copy"). Read-only; it
-- reads the role tables of 0.1.288 (schema 0098 or 0099) or casbin_rule
-- (0100 on), whichever the database has, and prints
--
--   <user id> TAB <permission key> TAB <scope>
--
-- once per grant however many roles give it, by user id, then key and
-- scope bytewise: the users the core lets act (status active, not
-- deleted), the permissions in force (status active), "*" for a grant over
-- every row, and "<id> TAB * TAB *" for the super admin. The permissions of
-- the virtual self role come from the code, not from the database, and are
-- not listed.
--
--   psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 -f 0.1.290-effective-permissions.sql > before.tsv
--
-- server/internal/migrations TestEffectivePermissionsScript runs both
-- queries around 0100.

\pset format unaligned
\pset tuples_only on
\pset fieldsep '\t'
\pset footer off

START TRANSACTION READ ONLY ISOLATION LEVEL REPEATABLE READ;

SELECT to_regclass('public.casbin_rule') IS NOT NULL AS casbin \gset

\if :casbin
SELECT DISTINCT u.id, p.v1 COLLATE "C" AS key, p.v2 COLLATE "C" AS scope
FROM casbin_rule g
JOIN users u ON g.v0 = 'u:' || u.id
JOIN casbin_rule p ON p.ptype = 'p' AND p.v0 = g.v1
LEFT JOIN permissions pm ON pm.key = p.v1
WHERE g.ptype = 'g' AND u.deleted_at IS NULL AND u.status = 'active'
  AND (p.v1 = '*' OR pm.status = 'active')
ORDER BY 1, 2, 3;
\else
WITH grants AS (
    SELECT ur.user_id, p.key, '*' AS scope, p.status
    FROM user_roles ur JOIN role_permissions rp ON rp.role_id = ur.role_id JOIN permissions p ON p.id = rp.permission_id
    UNION
    SELECT ur.user_id, p.key, rs.scope, p.status
    FROM user_roles ur JOIN role_permission_scopes rs ON rs.role_id = ur.role_id JOIN permissions p ON p.id = rs.permission_id
    UNION
    SELECT ur.user_id, '*', '*', 'active'
    FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE r.superuser
)
SELECT g.user_id, g.key COLLATE "C" AS key, g.scope COLLATE "C" AS scope
FROM grants g JOIN users u ON u.id = g.user_id
WHERE u.deleted_at IS NULL AND u.status = 'active' AND g.status = 'active'
ORDER BY 1, 2, 3;
\endif

COMMIT;

import { computed, type Ref } from 'vue'
import { pluginPermissionKey } from '@sub2api/host'
import type { NewPermissionRoles, PluginReview, ReviewRoleColumn, ReviewUserPermission } from '@/api/types'
import { keysFromSuffixes, riskSummary, toManifestGrant, type RoleKeys } from '@/authz/pluginGrants'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from '@/components/permission/useGrantMatrix'
import { asArray } from '../pluginUtil'

/** A review row as a matrix row: local keys made full, defaults (canonical suffixes) made full keys. */
function toMatrixPerm(pluginKey: string, p: ReviewUserPermission): MatrixPerm {
  const full = (local: string) => pluginPermissionKey(pluginKey, local)
  const action = full(p.key)
  const defaults = p.defaults && Object.fromEntries(Object.entries(p.defaults).map(([role, suffixes]) => [role, keysFromSuffixes(action, suffixes)]))
  return {
    action,
    local: p.key,
    label: p.label,
    description: p.description,
    risk: p.risk,
    group: p.group,
    requires: p.requires?.map(full),
    scope: p.scope,
    // A renamed row is granted by inheritance: it has no suggestion to restore.
    defaults: p.renamed ? undefined : defaults,
    inheritFrom: p.renamed ? p.inherit_from : undefined,
    status: 'active'
  }
}

/**
 * The permission matrix of the consent page (CONTRACTS new_permission_roles).
 * Its rows are the permissions the consent creates (new and not revived, so
 * an upgrade lists only those); its columns, starting values and locks are
 * the server's, worked out for the viewer (role_columns, cells): a cell
 * starts at what the consent gives without overriding the row (cells.grant:
 * defaults and inheritance), and one the viewer may not name is held for a
 * super admin's review whatever the row becomes (D2), or is critical (D3).
 * A renamed row is granted to the old key's holders whether or not it is
 * overridden (install inheritFrom), so it is shown as inherited and locked:
 * the roles are adjusted on the Roles page once installed.
 */
export function useConsentMatrix(review: Ref<PluginReview | null>) {
  const pluginKey = computed(() => review.value?.plugin_key ?? '')
  const rows = computed(() => asArray<ReviewUserPermission>(review.value?.user_permissions).filter((p) => p.new && !p.revived))
  const perms = computed(() => rows.value.map((p) => toMatrixPerm(pluginKey.value, p)))
  const roles = computed<MatrixRole[]>(() => asArray<ReviewRoleColumn>(review.value?.role_columns))
  const cells = computed(() => new Map(rows.value.map((p) => [p.key, p.cells ?? {}])))
  const cellOf = (p: MatrixPerm, r: MatrixRole) => cells.value.get(p.local)?.[r.key]

  const baseline = computed<RoleKeys>(() =>
    Object.fromEntries(roles.value.map((r) => [r.key, perms.value.flatMap((p) => keysFromSuffixes(p.action, cellOf(p, r)?.grant ?? []))]))
  )
  function lockOf(p: MatrixPerm, r: MatrixRole): CellLock {
    if (r.superuser) return 'superuser'
    const c = cellOf(p, r)
    if (!c?.nameable) return c?.reason === 'critical' ? 'critical' : c?.withheld ? 'pending_review' : 'role_readonly'
    return p.inheritFrom ? 'inherited' : null
  }
  const matrix = useGrantMatrix({ perms, roles, baseline, lockOf })

  /** The roles the viewer may name on a row: an overridden row lists them all, a role left out would get nothing. */
  const nameable = (p: MatrixPerm) => roles.value.filter((r) => lockOf(p, r) === null)

  /** new_permission_roles: the rows that differ from where they started, each with every role the viewer may name. */
  function payload(): NewPermissionRoles {
    const changed = new Set(matrix.changes.value.map((c) => c.action))
    return Object.fromEntries(
      perms.value
        .filter((p) => changed.has(p.action))
        .map((p) => [p.local, Object.fromEntries(nameable(p).map((r) => [r.key, toManifestGrant(p, matrix.cell(p, r)) ?? []]))])
    )
  }

  /** What the consent grants as the viewer leaves it: the cells it may name, inherited ones included (held cells go to review instead). */
  const granted = computed<RoleKeys>(() => Object.fromEntries(roles.value.map((r) => [r.key, perms.value.filter((p) => cellOf(p, r)?.nameable).flatMap((p) => matrix.cell(p, r))])))
  const riskOf = computed(() => perms.value.map((p) => ({ key: p.action, risk: p.risk })))
  const summary = computed(() => ({
    sensitive: riskSummary(granted.value, riskOf.value, 'sensitive'),
    critical: riskSummary(granted.value, riskOf.value, 'critical'),
    /** Held for review whether or not the row is overridden. */
    pending: perms.value.reduce((n, p) => n + roles.value.filter((r) => cellOf(p, r)?.withheld).length, 0)
  }))

  return { matrix, perms, roles, lockOf, payload, summary }
}

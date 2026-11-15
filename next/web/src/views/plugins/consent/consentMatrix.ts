import { computed, type Ref } from 'vue'
import { pluginPermissionKey } from '@sub2api/host'
import type { NewPermissionRoles, PluginReview, ReviewRoleColumn, ReviewUserPermission } from '@/api/types'
import { keysFromSuffixes, riskSummary, toManifestGrant, type RoleKeys } from '@/authz/pluginGrants'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from '@/components/permission/useGrantMatrix'
import { asArray } from '../pluginUtil'

/**
 * What a consent does to a declared user permission: creates it (new to the
 * catalog), keeps it (there already, grants as they are), or revives it
 * (removed before, back with its grants, D6). Only created ones are rows of
 * the matrix.
 */
export function permissionFate(p: Pick<ReviewUserPermission, 'new' | 'revived'>): 'created' | 'kept' | 'revived' {
  if (p.revived) return 'revived'
  return p.new ? 'created' : 'kept'
}

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
    // A renamed row starts from the old key's holders: it has no suggestion to restore.
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
 * A renamed row starts at the old key's holders (cells.grant) and is edited
 * like any other: the consent's row overrides the inheritance for the roles
 * the viewer may name (C2); it is marked "inherited from". Without role:read
 * the review leaves the holders out (C4): such a cell is locked, since an
 * override would replace grants the viewer cannot see.
 */
export function useConsentMatrix(review: Ref<PluginReview | null>) {
  const pluginKey = computed(() => review.value?.plugin_key ?? '')
  const declared = computed(() => asArray<ReviewUserPermission>(review.value?.user_permissions))
  const rows = computed(() => declared.value.filter((p) => permissionFate(p) === 'created'))
  const perms = computed(() => rows.value.map((p) => toMatrixPerm(pluginKey.value, p)))
  /** Every declared permission's name by full key: a row may require one the upgrade does not list. */
  const labels = computed(() => new Map(declared.value.map((p) => [pluginPermissionKey(pluginKey.value, p.key), p.label])))
  const roles = computed<MatrixRole[]>(() => asArray<ReviewRoleColumn>(review.value?.role_columns))
  const cells = computed(() => new Map(rows.value.map((p) => [p.key, p.cells ?? {}])))
  const cellOf = (p: MatrixPerm, r: MatrixRole) => cells.value.get(p.local)?.[r.key]

  const baseline = computed<RoleKeys>(() =>
    Object.fromEntries(roles.value.map((r) => [r.key, perms.value.flatMap((p) => keysFromSuffixes(p.action, cellOf(p, r)?.grant ?? []))]))
  )
  function lockOf(p: MatrixPerm, r: MatrixRole): CellLock {
    if (r.superuser) return 'superuser'
    const c = cellOf(p, r)
    // An inherited cell whose holders the viewer may not see (C4): it cannot tell what an override replaces.
    if (p.inheritFrom && c && c.grant === undefined && !c.withheld) return 'holders_hidden'
    if (c?.nameable) return null
    return c?.reason === 'critical' ? 'critical' : c?.withheld ? 'pending_review' : 'role_readonly'
  }
  const matrix = useGrantMatrix({ perms, roles, baseline, lockOf, labelOf: (action) => labels.value.get(action) })

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
  const summary = computed(() => ({
    sensitive: riskSummary(granted.value, perms.value, 'sensitive'),
    critical: riskSummary(granted.value, perms.value, 'critical'),
    /** Held for review whether or not the row is overridden. */
    pending: perms.value.reduce((n, p) => n + roles.value.filter((r) => cellOf(p, r)?.withheld).length, 0)
  }))

  return { matrix, perms, roles, lockOf, payload, summary }
}

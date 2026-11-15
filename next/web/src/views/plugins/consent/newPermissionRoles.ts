import { computed, type Ref } from 'vue'
import type { AssignableRole, NewPermissionRoles, PluginReview, ReviewRoleColumn, ReviewUserPermission } from '@/api/types'
import { useAssignableRoles } from '@/composables/useAssignableRoles'
import { useAuthStore } from '@/stores/auth'
import { keysFromSuffixes, riskSummary, toManifestGrant, type RoleKeys } from '@/authz/pluginGrants'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from '@/components/permission/useGrantMatrix'
import { asArray } from '../pluginUtil'

/**
 * The roles the caller may hand plugin permissions to (install
 * checkRolesGrantable: role:define, then CanGrantToRoles): the super admin
 * any role, others only roles on their managed list, within them, that they
 * do not hold themselves (built-in roles alike). The review of held-back
 * grants uses it; the consent page takes the server's role_columns / cells.
 */
export function useNameableRoles() {
  const auth = useAuthStore()
  const managed = useAssignableRoles()
  const choices = computed<AssignableRole[]>(() => {
    if (!auth.has('role:define')) return []
    if (auth.me?.superuser) return managed.roles.value
    return managed.roles.value.filter((r) => r.grantable && !auth.me?.roles.includes(r.key))
  })
  const canName = (role: string) => choices.value.some((c) => c.key === role)
  return { choices, canName }
}

/** A review row as a matrix row: local keys made full, defaults (canonical suffixes) made full keys. */
function toMatrixPerm(pluginKey: string, p: ReviewUserPermission): MatrixPerm {
  const full = (local: string) => `plugin.${pluginKey}:${local}`
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
 */
export function useConsentMatrix(review: Ref<PluginReview | null>) {
  const pluginKey = computed(() => review.value?.plugin_key ?? '')
  const rows = computed(() => asArray<ReviewUserPermission>(review.value?.user_permissions).filter((p) => p.new && !p.revived))
  const perms = computed(() => rows.value.map((p) => toMatrixPerm(pluginKey.value, p)))
  const roles = computed<MatrixRole[]>(() => asArray<ReviewRoleColumn>(review.value?.role_columns))
  const cellOf = (p: MatrixPerm, r: MatrixRole) => rows.value.find((x) => x.key === p.local)?.cells?.[r.key]

  const baseline = computed<RoleKeys>(() =>
    Object.fromEntries(roles.value.map((r) => [r.key, perms.value.flatMap((p) => keysFromSuffixes(p.action, cellOf(p, r)?.grant ?? []))]))
  )
  function lockOf(p: MatrixPerm, r: MatrixRole): CellLock {
    if (r.superuser) return 'superuser'
    const c = cellOf(p, r)
    if (c?.nameable) return null
    return c?.reason === 'critical' ? 'critical' : 'pending_review'
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

  /** What the consent grants as the viewer leaves it: the cells it may name (held cells go to review instead). */
  const granted = computed<RoleKeys>(() => Object.fromEntries(roles.value.map((r) => [r.key, perms.value.filter((p) => lockOf(p, r) === null).flatMap((p) => matrix.cell(p, r))])))
  const summary = computed(() => ({
    sensitive: riskSummary(granted.value, perms.value.map((p) => ({ key: p.action, risk: p.risk })), 'sensitive'),
    /** Held for review whether or not the row is overridden. */
    pending: perms.value.reduce((n, p) => n + roles.value.filter((r) => cellOf(p, r)?.withheld).length, 0)
  }))

  return { matrix, perms, roles, lockOf, payload, summary }
}

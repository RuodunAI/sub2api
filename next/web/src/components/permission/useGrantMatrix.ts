// The state of a permission × role matrix of one plugin, shared by the
// matrix component and the page that owns it (consent page, plugin detail
// "Permissions" tab): the page reads `value`, `changes` and `dirty` to save,
// the component edits through `setCell`, `setGroup` and `restore`. Built on
// authz/pluginGrants: `value` is role → full canonical keys of the plugin, the
// shape of the matrix PUT body and of roles[].permission_keys.
//
// Two marks are kept apart: "differs from the plugin's suggestion"
// (`differsFromSuggested`, the dot shown on all entries) and "not saved"
// (`changes` against `baseline`: the detail page's save bar, the consent
// page's payload rows).
import { computed, ref, watch, type Ref } from 'vue'
import type { LText, PermRisk, PluginScopeDecl } from '@/api/types'
import { defaultScopes, type ScopeOffer } from '@/authz/scopeChoice'
import { cellKeys, cellOfScopes, diffCells, narrows, sameKeys, suggestedOf, withCell, type RoleKeys } from '@/authz/pluginGrants'

/**
 * Why a cell cannot be edited freely; null when it can.
 * - superuser: the super admin column, fixed at everything.
 * - pending_review: install / upgrade, the installer may not name the role: a super admin reviews it (D2).
 * - critical: install / upgrade, a critical row for a non-super installer (D3).
 * - not_held: plugin detail, the editor does not hold the action: may only take away.
 * - role_readonly: plugin detail, the role is not editable (roles[].editable false).
 * - removed: the permission was removed: may only take away.
 */
export type CellLock = null | 'superuser' | 'pending_review' | 'critical' | 'not_held' | 'role_readonly' | 'removed'

/** Locks under which a cell may still be narrowed or cleared. */
const SHRINK_ONLY: ReadonlySet<CellLock> = new Set<CellLock>(['not_held', 'removed'])

/** One matrix row: a permission of the plugin, its keys full. */
export interface MatrixPerm {
  /** The full action key: plugin.<p>:<local>. */
  action: string
  local: string
  label: LText
  description?: LText
  risk: PermRisk
  group?: string
  /** Full keys. */
  requires?: string[]
  scope?: PluginScopeDecl
  /** The plugin's suggestion: role → full canonical keys; none (a renamed or critical row) suggests nothing. */
  defaults?: Readonly<Record<string, readonly string[] | undefined>>
  status: string
}

/** One matrix column. */
export interface MatrixRole {
  key: string
  name: LText
  builtin: boolean
  superuser: boolean
}

export interface GrantMatrixOptions {
  perms: Readonly<Ref<MatrixPerm[]>>
  roles: Readonly<Ref<MatrixRole[]>>
  /** The saved state; `value` starts from it and goes back to it whenever it changes. Consent: cells[].grant; detail: the roles' current keys. */
  baseline: Readonly<Ref<RoleKeys>>
  lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
}

export type GrantMatrix = ReturnType<typeof useGrantMatrix>

export function useGrantMatrix(opts: GrantMatrixOptions) {
  const value = ref<RoleKeys>({})
  const reset = () => (value.value = Object.fromEntries(Object.entries(opts.baseline.value).map(([role, keys]) => [role, [...keys]])))
  watch(opts.baseline, reset, { immediate: true })

  const suggested = computed(() => suggestedOf(opts.perms.value))
  const cell = (p: MatrixPerm, r: MatrixRole) => cellKeys(value.value[r.key] ?? [], p.action)

  /** A locked cell takes nothing, or (SHRINK_ONLY) only less than it holds. */
  const allows = (p: MatrixPerm, r: MatrixRole, next: readonly string[]) => {
    const lock = opts.lockOf(p, r)
    return lock === null || (SHRINK_ONLY.has(lock) && narrows(cell(p, r), next, p.action))
  }
  /** Writes cells of one role at once; the ones their lock refuses are skipped. */
  function write(r: MatrixRole, cells: Array<[MatrixPerm, string[]]>) {
    const held = value.value[r.key] ?? []
    const keys = cells.reduce((acc, [p, next]) => (allows(p, r, next) ? withCell(acc, p.action, next) : acc), held)
    if (!sameKeys(keys, held)) value.value = { ...value.value, [r.key]: keys }
  }

  function setCell(p: MatrixPerm, r: MatrixRole, keys: readonly string[]) {
    write(r, [[p, [...keys]]])
  }

  /**
   * Ticks (or clears) every row of a group for one role (D1: a group is a
   * shortcut, the keys stored are concrete). An unscoped row gets its key, a
   * scoped one the widest scope `offer` gives it; a row already granted is
   * left as it is, removed and locked rows are skipped.
   */
  function setGroup(group: string, r: MatrixRole, on: boolean, offer: (action: string) => ScopeOffer) {
    const rows = opts.perms.value.filter((p) => (p.group ?? '') === group && (!on || (p.status !== 'removed' && !cell(p, r).length)))
    write(r, rows.map((p) => [p, on ? (p.scope ? cellOfScopes(p.action, defaultScopes(offer(p.action))) : [p.action]) : []]))
  }

  /** Puts one row (or every row) back to the plugin's suggestion; locked cells and rows without one are left alone. */
  function restore(p?: MatrixPerm) {
    const rows = (p ? [p] : opts.perms.value).filter((x) => x.defaults)
    for (const r of opts.roles.value) {
      const editable = rows.filter((x) => opts.lockOf(x, r) === null)
      write(r, editable.map((x) => [x, cellKeys(suggested.value[r.key] ?? [], x.action)]))
    }
  }

  /** The cell differs from the plugin's suggestion; locked cells and rows without a suggestion never do (restore leaves them). */
  function differsFromSuggested(p: MatrixPerm, r: MatrixRole): boolean {
    if (!p.defaults || opts.lockOf(p, r) !== null) return false
    return !sameKeys(cell(p, r), cellKeys(suggested.value[r.key] ?? [], p.action))
  }

  const changes = computed(() =>
    diffCells(
      opts.baseline.value,
      value.value,
      opts.perms.value.map((p) => p.action),
      opts.roles.value.map((r) => r.key)
    )
  )
  const dirty = computed(() => changes.value.length > 0)

  return { value, suggested, cell, setCell, setGroup, restore, differsFromSuggested, changes, dirty, reset }
}

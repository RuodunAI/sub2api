// The state of a permission × role matrix of one plugin, shared by the
// matrix component and the page that owns it (consent page, plugin detail
// "Permissions" tab): the page reads `value`, `changes` and `dirty` to save,
// the component edits through `setCell`, `toggleGroup` and `restore`. Built on
// authz/pluginGrants: `value` is role → full canonical keys of the plugin, the
// shape of the matrix PUT body and of roles[].permission_keys.
//
// Two marks are kept apart: "differs from the plugin's suggestion"
// (`differsFromSuggested`, the dot shown on all entries) and "not saved"
// (`changes` against `baseline`: the detail page's save bar, the consent
// page's payload rows).
import { computed, ref, watch, type Ref } from 'vue'
import type { LText, PermissionItem } from '@/api/types'
import { defaultScopes, type ScopeOffer } from '@/authz/scopeChoice'
import { cellKeys, cellOfScopes, diffCells, isSuggestedRole, narrows, sameKeys, suggestedOf, withCell, type RoleKeys } from '@/authz/pluginGrants'

/**
 * Why a cell cannot be edited freely; null when it can.
 * - superuser: the super admin column, fixed at everything.
 * - pending_review: install / upgrade, the installer may not name the role and the cell's grant waits for a super admin's review (D2).
 * - critical: install / upgrade, a critical row for a non-super installer (D3).
 * - inherited: install / upgrade, a renamed row: the server grants the old key's holders whatever the consent says (inheritFrom).
 * - not_held: plugin detail, the editor does not hold the action: may only take away.
 * - role_readonly: the editor may not change the role (detail: roles[].editable false; consent: a role it may not name, nothing held).
 * - removed: the permission was removed: may only take away.
 */
export type CellLock = null | 'superuser' | 'pending_review' | 'critical' | 'inherited' | 'not_held' | 'role_readonly' | 'removed'

const SHRINK_ONLY: ReadonlySet<CellLock> = new Set<CellLock>(['not_held', 'removed'])
/** Under the lock a cell may still be narrowed or cleared. */
export const isShrinkOnly = (lock: CellLock) => SHRINK_ONLY.has(lock)
/** The lock freezes a cell outright. */
export const isFixed = (lock: CellLock) => lock !== null && !SHRINK_ONLY.has(lock)

/** One matrix row: a permission of the plugin, its keys full. */
export type MatrixPerm = Pick<PermissionItem, 'label' | 'description' | 'risk' | 'group' | 'requires' | 'scope' | 'status'> & {
  /** The full action key: plugin.<p>:<local>. */
  action: string
  local: string
  /** The plugin's suggestion: role → full canonical keys; none (a renamed or critical row) suggests nothing. */
  defaults?: Readonly<Record<string, readonly string[] | undefined>>
  /** A renamed row (consent): the local keys whose holders it is granted to. */
  inheritFrom?: string[]
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

  /** A locked cell takes nothing, or (shrink-only) only less than it holds. */
  const allows = (p: MatrixPerm, r: MatrixRole, next: readonly string[]) => {
    const lock = opts.lockOf(p, r)
    return lock === null || (isShrinkOnly(lock) && narrows(cell(p, r), next, p.action))
  }
  /** Writes cells of one role at once; the ones their lock refuses are skipped. */
  function write(r: MatrixRole, cells: ReadonlyArray<readonly [MatrixPerm, string[]]>) {
    const held = value.value[r.key] ?? []
    const keys = cells.reduce((acc, [p, next]) => (allows(p, r, next) ? withCell(acc, p.action, next) : acc), held)
    if (!sameKeys(keys, held)) value.value = { ...value.value, [r.key]: keys }
  }

  function setCell(p: MatrixPerm, r: MatrixRole, keys: readonly string[]) {
    write(r, [[p, [...keys]]])
  }

  /**
   * The rows of perms a group checkbox covers for one role, each with what
   * ticking gives it: the rows the role's cell may take freely (no lock, not
   * removed), an unscoped one its key, a scoped one the widest scope `offer`
   * gives (none offered: left out). The checkbox's state and its action both
   * go by these rows only.
   */
  function groupRows(perms: readonly MatrixPerm[], r: MatrixRole, offer: (action: string) => ScopeOffer) {
    return perms
      .filter((p) => p.status !== 'removed' && opts.lockOf(p, r) === null)
      .map((p) => [p, p.scope ? cellOfScopes(p.action, defaultScopes(offer(p.action))) : [p.action]] as const)
      .filter(([, on]) => on.length)
  }
  function groupState(perms: readonly MatrixPerm[], r: MatrixRole, offer: (action: string) => ScopeOffer): 'all' | 'some' | 'none' {
    const rows = groupRows(perms, r, offer)
    const n = rows.filter(([p]) => cell(p, r).length).length
    if (n === 0) return 'none'
    return n === rows.length ? 'all' : 'some'
  }
  /**
   * Ticks the group's rows for one role, or clears them once all are ticked
   * (D1: a group is a shortcut, the keys stored are concrete); ticking leaves
   * a row already granted as it is.
   */
  function toggleGroup(perms: readonly MatrixPerm[], r: MatrixRole, offer: (action: string) => ScopeOffer) {
    const on = groupState(perms, r, offer) !== 'all'
    const rows = groupRows(perms, r, offer).filter(([p]) => !on || !cell(p, r).length)
    write(r, rows.map(([p, keys]) => [p, on ? [...keys] : []]))
  }

  /** The cell has a suggestion to compare with and restore: a row with defaults, a role they speak for (built-in), no lock. */
  const suggests = (p: MatrixPerm, r: MatrixRole) => !!p.defaults && isSuggestedRole(r.key) && opts.lockOf(p, r) === null
  const suggestedCell = (p: MatrixPerm, r: MatrixRole) => cellKeys(suggested.value[r.key] ?? [], p.action)

  /** Puts one row (or every row) back to the plugin's suggestion, in the cells that have one (suggests). */
  function restore(p?: MatrixPerm) {
    const rows = p ? [p] : opts.perms.value
    for (const r of opts.roles.value) write(r, rows.filter((x) => suggests(x, r)).map((x) => [x, suggestedCell(x, r)]))
  }

  /** The cell differs from the plugin's suggestion; a cell without one (suggests) never does, restore leaving it alone. */
  function differsFromSuggested(p: MatrixPerm, r: MatrixRole): boolean {
    return suggests(p, r) && !sameKeys(cell(p, r), suggestedCell(p, r))
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

  return { value, cell, setCell, groupRows, groupState, toggleGroup, restore, differsFromSuggested, changes, dirty, reset }
}

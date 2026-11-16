// The state of a permission × role matrix of one plugin, shared by the
// matrix component and the page that owns it (consent page, plugin detail
// "Permissions" tab): the page hands it the rows, columns, locks and the
// editor's scopes once and reads `value`, `changes` and `dirty` to save; the
// component renders from it alone and edits through `setCell`, `toggleGroup`
// and `restore`. Built on authz/pluginGrants: `value` is role → full
// canonical keys of the plugin, the shape of the matrix PUT body and of
// roles[].permission_keys.
//
// Two marks are kept apart: "differs from the plugin's suggestion"
// (`differs`, the mark shown on all entries; the role editor answers alike
// through pluginGrants.suggestionState) and "not saved" (`changes` against
// `baseline`: the detail page's save bar, the consent page's payload rows).
import { computed, ref, shallowRef, watch, type Ref } from 'vue'
import type { LText, PermissionItem } from '@/api/types'
import { lt } from '@/i18n'
import { ALL_SCOPE, type Scope } from '@/authz/implies'
import { defaultScopes, offerOf, type ScopeOffer } from '@/authz/scopeChoice'
import {
  cellKeys,
  cellOfScopes,
  diffCells,
  grantableWithin,
  isSuggestedRole,
  narrows,
  replayCells,
  sameKeys,
  suggestedOf,
  suggestionState,
  withCell,
  type Replay,
  type RoleKeys,
  type SuggestionState
} from '@/authz/pluginGrants'

/**
 * Why a cell cannot be edited freely; null when it can.
 * - superuser: the super admin column, fixed at everything.
 * - pending_review: install / upgrade, the installer may not name the role and the cell's grant waits for a super admin's review (D2).
 * - critical: install / upgrade, a critical row for a non-super installer (D3).
 * - not_held: plugin detail, the editor does not hold the action: may only take away.
 * - role_readonly: the editor may not change the role (detail: roles[].editable false; consent: a role it may not name, nothing held).
 * - removed: the permission was removed: may only take away.
 */
export type CellLock = null | 'superuser' | 'pending_review' | 'critical' | 'not_held' | 'role_readonly' | 'removed' | 'holders_hidden'

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
  /**
   * A renamed row (consent): the local keys whose holders it starts granted
   * to; its cells are edited like any other (the consent's row overrides the
   * inheritance, C2).
   */
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
  /**
   * The saved state; `value` starts from it. Consent: cells[].grant; detail:
   * the roles' current keys. When it changes (a reload, a save, a conflict),
   * the cells changed locally are put on top of it again: the edits survive
   * a refresh that brings others' writes.
   */
  baseline: Readonly<Ref<RoleKeys>>
  lockOf: (perm: MatrixPerm, role: MatrixRole) => CellLock
  /**
   * The editor's scopes of an action (detail page: what it holds): what a
   * scoped cell offers, what a group ticks and what a restore may write.
   * Left out, everything (consent page: the permissions it creates need not
   * be held; the server's cells.nameable decides, as lockOf).
   */
  scopeLimit?: (action: string) => Scope[]
  /** The name of a permission that may not be a row (an upgrade lists only the new ones): the requires hints. */
  labelOf?: (action: string) => LText | undefined
  /** Nothing may change (no role:define). */
  readOnly?: Readonly<Ref<boolean>>
}

/** One cell's state, worked out once per cell. */
export interface CellState {
  lock: CellLock
  /** The keys it shows: everything for the super admin, else its keys. */
  keys: string[]
  /** Frozen outright (or the whole matrix read-only). */
  fixed: boolean
  /** Fixed, or empty under a take-away-only lock. */
  disabled: boolean
  /** Against the plugin's suggestion; null where none speaks for the cell. */
  suggestion: SuggestionState | null
}

export type GrantMatrix = ReturnType<typeof useGrantMatrix>

export function useGrantMatrix(opts: GrantMatrixOptions) {
  const { perms, roles, lockOf } = opts
  const scopeLimit = opts.scopeLimit ?? (() => [ALL_SCOPE])
  const readOnly = computed(() => !!opts.readOnly?.value)
  const mayGrant = grantableWithin(scopeLimit)
  const offerFor = (action: string): ScopeOffer => offerOf(scopeLimit(action), action)

  const value = ref<RoleKeys>({})
  const reset = () => (value.value = Object.fromEntries(Object.entries(opts.baseline.value).map(([role, keys]) => [role, [...keys]])))

  const suggested = computed(() => suggestedOf(perms.value))
  const cell = (p: MatrixPerm, r: MatrixRole) => cellKeys(value.value[r.key] ?? [], p.action)

  /** A locked cell takes nothing, or (shrink-only) only less than it holds (from: what it holds, its current keys by default). */
  const allows = (p: MatrixPerm, r: MatrixRole, next: readonly string[], from: readonly string[] = cell(p, r)) => {
    const lock = lockOf(p, r)
    return !readOnly.value && (lock === null || (isShrinkOnly(lock) && narrows(from, next, p.action)))
  }
  /** Writes cells of one role at once; the ones their lock refuses are skipped. */
  function write(r: MatrixRole, cells: ReadonlyArray<readonly [MatrixPerm, readonly string[]]>) {
    const held = value.value[r.key] ?? []
    const keys = cells.reduce((acc, [p, next]) => (allows(p, r, next) ? withCell(acc, p.action, next) : acc), held)
    if (!sameKeys(keys, held)) value.value = { ...value.value, [r.key]: keys }
  }

  /** The changes of `value` against base, over the current rows and columns. */
  const diffFrom = (base: RoleKeys) =>
    diffCells(
      base,
      value.value,
      perms.value.map((p) => p.action),
      roles.value.map((r) => r.key)
    )

  /**
   * The last merge of the unsaved cells onto a new baseline (a reload, or a
   * 409 read again): what was kept, dropped by a lock that came in, or kept
   * over someone else's change of the same cell. Null until a baseline is
   * replaced while cells are unsaved.
   */
  const replayed = shallowRef<Replay | null>(null)
  watch(
    opts.baseline,
    (next, prev) => {
      const edited = value.value
      reset()
      if (!prev) return
      const roleOf = new Map(roles.value.map((r) => [r.key, r]))
      const permOf = new Map(perms.value.map((p) => [p.action, p]))
      const actions = perms.value.map((p) => p.action)
      const r = replayCells(prev, edited, value.value, actions, [...roleOf.keys()], (c, now) => {
        const p = permOf.get(c.action)
        const role = roleOf.get(c.role)
        return !!p && !!role && allows(p, role, c.to, now)
      })
      value.value = r.value
      replayed.value = r.kept.length || r.dropped.length ? r : null
    },
    { immediate: true }
  )

  function setCell(p: MatrixPerm, r: MatrixRole, keys: readonly string[]) {
    write(r, [[p, keys]])
  }

  /**
   * The rows of perms a group checkbox covers for one role, each with what
   * ticking gives it: the rows the role's cell may take freely (no lock, not
   * removed, not critical: D1 makes a group a shortcut for its ordinary
   * rows, a critical one is ticked on its own), an unscoped one its key, a
   * scoped one the widest scope offered (none offered: left out). The
   * checkbox's state and its action both go by these rows only.
   */
  function groupRows(rows: readonly MatrixPerm[], r: MatrixRole) {
    return rows
      .filter((p) => p.status !== 'removed' && p.risk !== 'critical' && lockOf(p, r) === null)
      .map((p) => [p, p.scope ? cellOfScopes(p.action, defaultScopes(offerFor(p.action))) : [p.action]] as const)
      .filter(([, on]) => on.length)
  }
  function groupState(rows: readonly MatrixPerm[], r: MatrixRole): 'all' | 'some' | 'none' {
    const covered = groupRows(rows, r)
    const n = covered.filter(([p]) => cell(p, r).length).length
    if (n === 0) return 'none'
    return n === covered.length ? 'all' : 'some'
  }
  /**
   * Ticks the group's rows for one role, or clears them once all are ticked
   * (D1: a group is a shortcut, the keys stored are concrete); ticking leaves
   * a row already granted as it is.
   */
  function toggleGroup(rows: readonly MatrixPerm[], r: MatrixRole) {
    const on = groupState(rows, r) !== 'all'
    const covered = groupRows(rows, r).filter(([p]) => !on || !cell(p, r).length)
    write(r, covered.map(([p, keys]) => [p, on ? keys : []]))
  }

  /** A suggestion speaks for the cell: a row with defaults, not removed, a role they name (built-in), not frozen. */
  const suggests = (p: MatrixPerm, r: MatrixRole, lock: CellLock) => !!p.defaults && p.status !== 'removed' && isSuggestedRole(r.key) && !isFixed(lock)
  const suggestedCell = (p: MatrixPerm, r: MatrixRole) => cellKeys(suggested.value[r.key] ?? [], p.action)

  const cellId = (p: MatrixPerm, r: MatrixRole) => `${p.action}\u0000${r.key}`
  const states = computed(() => {
    const out = new Map<string, CellState>()
    for (const p of perms.value) {
      for (const r of roles.value) {
        const lock = lockOf(p, r)
        const keys = lock === 'superuser' ? [p.action] : cell(p, r)
        const fixed = readOnly.value || isFixed(lock)
        out.set(cellId(p, r), {
          lock,
          keys,
          fixed,
          disabled: fixed || (lock !== null && !keys.length),
          suggestion: suggests(p, r, lock) ? suggestionState(keys, suggestedCell(p, r), mayGrant) : null
        })
      }
    }
    return out
  })
  const state = (p: MatrixPerm, r: MatrixRole) => states.value.get(cellId(p, r))!
  /** The cell differs from the plugin's suggestion and a restore can put it back. */
  const differs = (p: MatrixPerm, r: MatrixRole) => state(p, r).suggestion === 'differs'
  const differing = computed(() => [...states.value.values()].filter((v) => v.suggestion === 'differs').length)

  /**
   * Puts one row (or every row) back to the plugin's suggestion where a
   * restore can (pluginGrants.restoreCell); returns how many cells were out
   * of the editor's reach and left as they are.
   */
  function restore(p?: MatrixPerm): number {
    const rows = p ? [p] : perms.value
    let skipped = 0
    for (const r of roles.value) {
      const cells = rows.flatMap((x) => {
        const st = state(x, r).suggestion
        if (st === 'unreachable') skipped++
        return st === 'differs' ? [[x, suggestedCell(x, r)] as const] : []
      })
      write(r, cells)
    }
    return skipped
  }

  /** The names of a row's requires, from the rows or labelOf; a key unknown to both as it is. */
  const requiresLabels = (p: MatrixPerm) =>
    (p.requires ?? []).map((k) => lt(perms.value.find((x) => x.action === k)?.label ?? opts.labelOf?.(k)) || k)

  const changes = computed(() => diffFrom(opts.baseline.value))
  const dirty = computed(() => changes.value.length > 0)

  return {
    perms,
    roles,
    lockOf,
    readOnly,
    offerFor,
    value,
    replayed,
    cell,
    state,
    setCell,
    groupRows,
    groupState,
    toggleGroup,
    restore,
    differs,
    differing,
    requiresLabels,
    changes,
    dirty,
    reset
  }
}

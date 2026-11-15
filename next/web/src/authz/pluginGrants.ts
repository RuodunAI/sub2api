// A plugin's permissions granted to roles, as the permission matrix (consent
// page, plugin detail) and the role editor's plugin modules share them: role →
// the role's full canonical keys of the plugin (RoleKeys). It is the shape of
// GET/PUT /permissions/plugins/:plugin (roles[].permission_keys, the PUT body)
// and of the catalog's defaults, so nothing converts on the way in or out. A
// cell is the keys of one action ("plugin.m:flow:manage" plus its scoped keys)
// of one role. scopeChoice.withScopes edits one action of one set; these
// compare and sum many roles × actions. Pure.
import { canonicalPermission, parsePermission, permissionIndex, patternKey, reduceScopes, scopeImplies, scopeSuffix, type Scope } from './implies'
import { choiceOf, withScopes } from './scopeChoice'
import type { ManifestGrant, PermissionItem, PermRisk } from '../api/types'

/** Role → the role's full canonical keys of one plugin. */
export type RoleKeys = Record<string, string[]>

/** The roles a plugin's suggestion (manifest defaults) may name: the built-in ones but the super admin. */
export const SUGGESTED_ROLES = ['admin', 'operator', 'supplier', 'user'] as const satisfies readonly (keyof NonNullable<PermissionItem['defaults']>)[]
export type SuggestedRole = (typeof SUGGESTED_ROLES)[number]

/**
 * Whether the plugin's suggestion speaks for role: a custom role's cells are
 * its own (no suggestion names it, so restoring or comparing would only clear
 * them). Built-in keys are reserved, so the key alone tells.
 */
export const isSuggestedRole = (role: string): role is SuggestedRole => (SUGGESTED_ROLES as readonly string[]).includes(role)

/** The plugin-local part of a full key ("flow:read" of "plugin.m:flow:read"). */
export const localOf = (key: string) => key.slice(key.indexOf(':') + 1)

/** The action of a key ("plugin.m:flow:manage" of "plugin.m:flow:manage:own"); a key that does not parse is its own. */
export const actionOf = (key: string) => parsePermission(key)?.action ?? key

/** keys canonical, without duplicates, sorted; a key that does not parse is kept as it is (the server refuses it). */
const canonicalSet = (keys: readonly string[]) => [...new Set(keys.map((k) => canonicalPermission(k) ?? k))].sort()

/** The keys of action among keys: one cell. */
export function cellKeys(keys: readonly string[], action: string): string[] {
  return keys.filter((k) => actionOf(k) === action)
}

/** The scopes of the action's cell, without those another implies. */
export function scopesOfCell(keys: readonly string[], action: string): Scope[] {
  return reduceScopes(keys.flatMap((k) => {
    const p = parsePermission(k)
    return p?.action === action ? [p.scope] : []
  }))
}

/** The cell granting action at scopes (none: not granted), canonical. */
export function cellOfScopes(action: string, scopes: readonly Scope[]): string[] {
  return canonicalSet(reduceScopes(scopes).map((scope) => patternKey({ action, scope })))
}

/** keys with the action's cell replaced by next; canonical, without duplicates, sorted. */
export function withCell(keys: readonly string[], action: string, next: readonly string[]): string[] {
  return canonicalSet([...withScopes(new Set(keys), action, scopesOfCell(next, action))])
}

/** Whether a and b grant the same keys, spelling and order aside ("a:*" is "a"). */
export function sameKeys(a: readonly string[], b: readonly string[]): boolean {
  const x = canonicalSet(a)
  const y = canonicalSet(b)
  return x.length === y.length && x.every((k, i) => k === y[i])
}

/** Whether every scope of next is implied by one of from: next only takes away. */
export function narrows(from: readonly string[], next: readonly string[], action: string): boolean {
  const held = scopesOfCell(from, action)
  return scopesOfCell(next, action).every((s) => held.some((h) => scopeImplies(h, s)))
}

/** The plugin's suggestion for every role, merged from the permissions' defaults (full keys). */
export function suggestedOf(perms: readonly { defaults?: Readonly<Record<string, readonly string[] | undefined>> }[]): RoleKeys {
  const out: RoleKeys = {}
  for (const p of perms) {
    for (const [role, keys] of Object.entries(p.defaults ?? {})) out[role] = [...(out[role] ?? []), ...(keys ?? [])]
  }
  for (const role of Object.keys(out)) out[role] = canonicalSet(out[role])
  return out
}

/** One cell that differs between two RoleKeys. */
export interface CellChange {
  action: string
  role: string
  from: string[]
  to: string[]
}

/** The cells of actions × roles that differ from base in next, one change each. */
export function diffCells(base: RoleKeys, next: RoleKeys, actions: readonly string[], roles: readonly string[]): CellChange[] {
  return roles.flatMap((role) =>
    actions.flatMap((action) => {
      const from = cellKeys(base[role] ?? [], action)
      const to = cellKeys(next[role] ?? [], action)
      return sameKeys(from, to) ? [] : [{ action, role, from, to }]
    })
  )
}

/** How many permissions of the risk value grants, and to which roles ("N sensitive permissions to Admin, Operator"). */
export function riskSummary(value: RoleKeys, perms: readonly { action: string; risk: PermRisk }[], risk: PermRisk): { perms: number; roles: string[] } {
  const actions = new Set(perms.filter((p) => p.risk === risk).map((p) => p.action))
  const granted = new Set<string>()
  const roles: string[] = []
  for (const [role, keys] of Object.entries(value)) {
    const held = keys.map(actionOf).filter((a) => actions.has(a))
    held.forEach((a) => granted.add(a))
    if (held.length) roles.push(role)
  }
  return { perms: granted.size, roles }
}

/** A cell of a sensitive or critical permission that a change grants more of. */
export interface RiskyGrant {
  action: string
  role: string
  risk: Exclude<PermRisk, 'low'>
}

/**
 * The cells of sensitive and critical permissions next grants a key in that
 * base does not imply (a new or wider grant; narrowing or taking away is not), critical first:
 * what a save asks to confirm (D1: a group ticks no critical row, but a row
 * ticked one by one still lands here).
 */
export function newRiskyGrants(base: RoleKeys, next: RoleKeys, perms: readonly { action: string; risk: PermRisk }[]): RiskyGrant[] {
  const out: RiskyGrant[] = []
  for (const p of perms) {
    if (p.risk === 'low') continue
    for (const [role, keys] of Object.entries(next)) {
      const before = permissionIndex(cellKeys(base[role] ?? [], p.action))
      if (cellKeys(keys, p.action).some((k) => !before.has(k))) out.push({ action: p.action, role, risk: p.risk })
    }
  }
  return out.sort((a, b) => Number(b.risk === 'critical') - Number(a.risk === 'critical'))
}

/**
 * Whether the editor may grant key: one of its scopes of the key's action
 * (scopeLimit, auth.scopes on the detail page) implies the key's scope.
 */
export const grantableWithin = (scopeLimit: (action: string) => readonly Scope[]) => (key: string) => {
  const p = parsePermission(key)
  return !!p && scopeLimit(p.action).some((l) => scopeImplies(l, p.scope))
}

/**
 * A cell put back to want (the plugin's suggestion), or null when the editor
 * cannot do it whole: a key of want it does not hold already that mayGrant
 * refuses (the server would refuse the save). Such a cell is left as it is,
 * neither cut down to what the editor may grant (that would take away grants
 * the suggestion keeps) nor written past the editor. The role editor and the
 * matrix both restore through it.
 */
export function restoreCell(held: readonly string[], want: readonly string[], mayGrant: (key: string) => boolean): string[] | null {
  const have = new Set(canonicalSet(held))
  const next = canonicalSet(want)
  return next.every((k) => have.has(k) || mayGrant(k)) ? next : null
}

/**
 * How a cell stands against the plugin's suggestion: the same, differing
 * (restoreCell can put it back), or out of the editor's reach (not marked,
 * not counted, skipped by a restore).
 */
export type SuggestionState = 'same' | 'differs' | 'unreachable'
export function suggestionState(held: readonly string[], want: readonly string[], mayGrant: (key: string) => boolean): SuggestionState {
  if (sameKeys(held, want)) return 'same'
  return restoreCell(held, want, mayGrant) ? 'differs' : 'unreachable'
}

/** A cell as one choice: none, every row, the own rows, or chosen values (refs: how many plugin values). */
export function cellSummary(keys: readonly string[], action: string): { choice: 'none' | 'all' | 'own' | 'custom'; refs: number } {
  const scopes = scopesOfCell(keys, action)
  const c = choiceOf(scopes)
  const refs = c.refs.length
  if (!scopes.length) return { choice: 'none', refs }
  if (c.all) return { choice: 'all', refs }
  return { choice: scopes.length === 1 && c.own ? 'own' : 'custom', refs }
}

/** Canonical suffixes (the review's cells[].grant and defaults) → full keys: "" is the bare action, the rest action:<suffix>. */
export function keysFromSuffixes(action: string, suffixes: readonly string[]): string[] {
  return canonicalSet(suffixes.map((s) => (s ? `${action}:${s}` : action)))
}

/**
 * A cell as the consent's new_permission_roles names it: true for an
 * unscoped permission; its scopes for a scoped one ("*" for all, else "own",
 * "value/input", "flow/3"); null when not granted (the role is left out).
 */
export function toManifestGrant(p: { action: string; scope?: unknown }, keys: readonly string[]): ManifestGrant | null {
  const scopes = scopesOfCell(keys, p.action)
  if (!scopes.length) return null
  if (!p.scope) return true
  return scopes.map((s) => (s.all ? '*' : scopeSuffix(p.action, s))).sort()
}

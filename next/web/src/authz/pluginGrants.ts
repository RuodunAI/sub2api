// A plugin's permissions granted to roles, as the permission matrix (consent
// page, plugin detail) and the role editor's plugin modules share them: role →
// the role's full canonical keys of the plugin (RoleKeys). It is the shape of
// GET/PUT /permissions/plugins/:plugin (roles[].permission_keys, the PUT body)
// and of the catalog's defaults, so nothing converts on the way in or out. A
// cell is the keys of one action ("plugin.m:flow:manage" plus its scoped keys)
// of one role. scopeChoice.withScopes edits one action of one set; these
// compare and sum many roles × actions. Pure.
import { canonicalPermission, parsePermission, patternKey, reduceScopes, scopeImplies, scopeSuffix, type Scope } from './implies'
import { withScopes } from './scopeChoice'
import type { ManifestGrant, PermRisk } from '../api/types'

/** Role → the role's full canonical keys of one plugin. */
export type RoleKeys = Record<string, string[]>

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

/** How many permissions of the risk value grants, and to how many roles ("N sensitive permissions to M roles"). */
export function riskSummary(value: RoleKeys, perms: readonly { key: string; risk: PermRisk }[], risk: PermRisk): { perms: number; roles: number } {
  const actions = new Set(perms.filter((p) => p.risk === risk).map((p) => p.key))
  const granted = new Set<string>()
  let roles = 0
  for (const keys of Object.values(value)) {
    const held = keys.map(actionOf).filter((a) => actions.has(a))
    held.forEach((a) => granted.add(a))
    if (held.length) roles++
  }
  return { perms: granted.size, roles }
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

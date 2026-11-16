// Scoped permission keys (CONTRACTS §21, wildcard permissions), the console's
// copy of server/internal/perm/pattern.go: an action key may end in a scope,
// "<action>:<scope>", that limits the rows it reaches:
//
//   scope = "*" | "own" | "team" | path
//   path  = account actions: "group/" seg [ "/account/" seg ] | "account/" id
//           proxy actions:   "proxy/" id
//           plugin actions:  resource "/" value
//   seg   = id | "*"
//
// "team" (CONTRACTS §102) is the caller's rows and those of the users below
// it; only core actions take it, and user actions no other scope but "*".
// A missing scope is "*": every row. The last segment of a key is a scope
// only when it is "*", "own", holds a "/", or is "team" after a scopable
// core action. A plugin action ("plugin.…")
// takes its scope by syntax alone, with no catalog: which resource, own or
// values it accepts is the server's to check when a grant is written. Both
// copies run the shared vectors of next/sdk/permvectors/vectors.json
// (implies.spec.ts), so they cannot drift. Pure: no Vue, no store (the mock server imports it too).
// Relative: the mock server (tsconfig.node) imports this module without the @ alias.
import { SCOPE_TYPE, type ScopableKey } from '../generated/permissions'

/** The row kind the scope of an action ranges over (the catalog's scope_type). */
export type ScopeResource = (typeof SCOPE_TYPE)[ScopableKey]

/** The actions that take a scope, with their resource (perm.scopable; the generated catalog). */
export const SCOPABLE: Readonly<Record<string, ScopeResource>> = SCOPE_TYPE

/** Whether the scope of action may name rows (an id range means nothing for creating a row). */
export const takesRowIds = (action: string) => action !== 'account:create'

/** Whether action takes the team scope: a core scoped action, but account:create (a new row is always the creator's own). */
export const takesTeam = (action: string) => !!SCOPABLE[action] && takesRowIds(action)

/** A group segment of "*": any group. */
export const WILDCARD = -1

/**
 * A parsed, canonical scope. At most one of all, own and team is set, and
 * then group and id are 0. A plugin scope sets ref only. Otherwise group > 0 is
 * group/<group>, WILDCARD is group/*, 0 no group part; id > 0 is the row
 * (account/<id>, proxy/<id>, or after a group part group/<g>/account/<id>).
 */
export interface Scope {
  all: boolean
  own: boolean
  /** The holder's rows and those of the users below it (users.owner_id). */
  team?: boolean
  group: number
  id: number
  /** A plugin scope "<resource>/<value>" ("flow/3", "value/input"); never set on a core scope. */
  ref?: string
}

/** A canonical key split into action and scope; unscoped keys have scope.all. */
export interface Pattern {
  action: string
  scope: Scope
}

export const ALL_SCOPE: Scope = Object.freeze({ all: true, own: false, group: 0, id: 0 })
export const OWN_SCOPE: Scope = Object.freeze({ all: false, own: true, group: 0, id: 0 })
export const TEAM_SCOPE: Scope = Object.freeze({ all: false, own: false, team: true, group: 0, id: 0 })
const MAX_ID = 9223372036854775807n
/** perm.scopeResourceRe "/" perm.scopeValueRe. */
const PLUGIN_REF = /^[a-z][a-z0-9_]{0,29}\/[A-Za-z0-9_.-]{1,64}$/

/** The plugin scope "<resource>/<value>". */
export const refScope = (ref: string): Scope => ({ all: false, own: false, group: 0, id: 0, ref })

/** Reads a key, trailing wildcards included, into its canonical pattern; null when it does not parse. */
export function parsePermission(k: string): Pattern | null {
  const i = k.lastIndexOf(':')
  const last = k.slice(i + 1)
  const action = k.slice(0, i)
  const resource = SCOPABLE[action]
  if (i < 0 || (last !== '*' && last !== 'own' && !last.includes('/') && (last !== 'team' || !resource))) return { action: k, scope: ALL_SCOPE }
  if (action.startsWith('plugin.')) return pluginPattern(action, last)
  const scope = resource && parseScope(resource, !takesRowIds(action), last)
  return scope ? { action, scope } : null
}

/** The canonical spelling of key (trailing "/*" dropped, a scope of everything left out); null when invalid. */
export function canonicalPermission(key: string): string | null {
  const p = parsePermission(key)
  return p && patternKey(p)
}

/** A plugin action takes "*", "own" or "<resource>/<value>", nothing else (no trailing wildcard). */
function pluginPattern(action: string, last: string): Pattern | null {
  if (last === '*') return { action, scope: ALL_SCOPE }
  if (last === 'own') return { action, scope: OWN_SCOPE }
  return PLUGIN_REF.test(last) ? { action, scope: refScope(last) } : null
}

function parseScope(resource: ScopeResource, create: boolean, raw: string): Scope | null {
  if (raw === '*') return ALL_SCOPE
  // A new row is always the creator's own; a user is owned by another.
  if (raw === 'team') return create ? null : TEAM_SCOPE
  if (raw === 'own') return resource === 'user' ? null : OWN_SCOPE
  if (resource === 'user') return null
  const parts = trimWildcards(resource, raw.split('/'))
  if (parts.length === 0) return ALL_SCOPE
  let group = 0
  let id: number | null = 0
  if (resource === 'proxy') {
    if (parts.length !== 2 || parts[0] !== 'proxy') return null
    id = parseId(parts[1], false)
  } else if (parts.length === 2 && parts[0] === 'account') {
    id = parseId(parts[1], false)
  } else if (parts[0] === 'group' && (parts.length === 2 || (parts.length === 4 && parts[2] === 'account'))) {
    const g = parseId(parts[1], true)
    if (g === null) return null
    group = g
    if (parts.length === 4) id = parseId(parts[3], false)
  } else {
    return null
  }
  // An id range means nothing for creating a row.
  if (id === null || (create && id !== 0)) return null
  return { all: false, own: false, group, id }
}

/**
 * Drops the trailing "/*" of a scope path: a lone "*" ("group/12/*") and the
 * row type of the resource with "*" ("group/12/account/*", "account/*"), but
 * not a group's "*" (group/* is any group) and nothing else.
 */
function trimWildcards(resource: ScopeResource, parts: string[]): string[] {
  let n = parts.length
  while (n > 0 && parts[n - 1] === '*') {
    if (n % 2 === 1) n -= 1
    else if (parts[n - 2] === resource) n -= 2
    else break
  }
  return parts.slice(0, n)
}

/** A positive decimal int64 without leading zeros, or "*" (WILDCARD) when allowed; null otherwise. */
function parseId(raw: string, wildcard: boolean): number | null {
  if (wildcard && raw === '*') return WILDCARD
  if (!/^[1-9]\d{0,18}$/.test(raw) || BigInt(raw) > MAX_ID) return null
  return Number(raw)
}

/** The canonical scope text of a pattern of action: '' for everything (perm.Pattern.Suffix). */
export function scopeSuffix(action: string, s: Scope): string {
  if (s.all) return ''
  if (s.own) return 'own'
  if (s.team) return 'team'
  if (s.ref) return s.ref
  const seg = (n: number) => (n === WILDCARD ? '*' : String(n))
  const row = s.id ? `${SCOPABLE[action]}/${seg(s.id)}` : ''
  if (!s.group) return row
  return row ? `group/${seg(s.group)}/${row}` : `group/${seg(s.group)}`
}

/** The canonical key of p. */
export function patternKey(p: Pattern): string {
  const suffix = scopeSuffix(p.action, p.scope)
  return suffix ? `${p.action}:${suffix}` : p.action
}

/**
 * Whether holding a scope grants want's (same action assumed): judged by the
 * text alone, never by where rows are now (group/12 does not imply
 * account/125 even while account 125 is in group 12). perm.Implies.
 */
export function scopeImplies(h: Scope, w: Scope): boolean {
  if (h.all) return true
  if (w.all) return false
  if (h.team) return !!w.team || w.own
  if (w.team) return false
  if (h.own || w.own) return h.own && w.own
  if (h.ref || w.ref) return h.ref === w.ref
  if (!h.group) return h.id === w.id // account/X or proxy/X: that row, with or without a group
  if (!w.group) return false // a group range never covers an ungrouped id
  if (h.group !== WILDCARD && h.group !== w.group) return false
  return !h.id || h.id === w.id
}

/** Whether holding have grants want. */
export function implies(have: Pattern, want: Pattern): boolean {
  return have.action === want.action && scopeImplies(have.scope, want.scope)
}

/** A row a scope is judged against: its id, owner and (accounts) groups. */
export interface ScopedRow {
  id?: number | null
  /** The creator: the owner own and team check on a row without owner_id (a proxy). */
  created_by?: number | null
  /** The owner of a row that has one apart from its creator (an account, CONTRACTS §102.4). */
  owner_id?: number | null
  group_ids?: readonly number[] | null
}

/** The user own and team check row against: its owner_id where it has one, else its creator. */
export const rowOwner = (row: ScopedRow): number | null | undefined => (row.owner_id !== undefined ? row.owner_id : row.created_by)

/**
 * Whether a scope reaches row for the caller me (core.DataScope.AllowsRow).
 * Only the server knows a team (core.ResolveScope): here it reaches what own
 * does (the mock adds the team it holds; the console, useScope.can, takes
 * any row the server listed).
 */
export function scopeAllowsRow(s: Scope, row: ScopedRow | null | undefined, me: number | undefined): boolean {
  if (s.all) return true
  if (!row) return false
  if (s.own || s.team) return me !== undefined && rowOwner(row) === me
  if (s.ref) return false // the plugin judges its own rows
  if (s.id && row.id !== s.id) return false
  if (!s.group) return !!s.id
  const groups = row.group_ids ?? []
  return s.group === WILDCARD ? groups.length > 0 : groups.includes(s.group)
}

/** What a list of held keys grants (the console's PermissionSet). */
export interface PermissionIndex {
  /** One of the held keys implies key (a key that does not parse is not held). */
  has(key: string): boolean
  /** The action is held at any scope. */
  hasAction(action: string): boolean
  /** The scopes held for action, without those another one implies; [] when not held. */
  scopes(action: string): Scope[]
}

/** Indexes held keys (canonical or not; invalid ones are ignored). */
export function permissionIndex(keys: readonly string[]): PermissionIndex {
  const byAction = new Map<string, Scope[]>()
  for (const k of keys) {
    const p = parsePermission(k)
    if (!p) continue
    byAction.set(p.action, [...(byAction.get(p.action) ?? []), p.scope])
  }
  for (const [action, list] of byAction) byAction.set(action, reduceScopes(list))
  return {
    has(key) {
      const want = parsePermission(key)
      return !!want && (byAction.get(want.action) ?? []).some((h) => scopeImplies(h, want.scope))
    },
    hasAction: (action) => byAction.has(action),
    scopes: (action) => byAction.get(action) ?? []
  }
}

/** list without duplicates and without a scope another one implies. */
export function reduceScopes(list: readonly Scope[]): Scope[] {
  const out: Scope[] = []
  list.forEach((s, i) => {
    const covered = list.some((o, j) => j !== i && scopeImplies(o, s) && (!scopeImplies(s, o) || j < i))
    if (!covered) out.push(s)
  })
  return out
}

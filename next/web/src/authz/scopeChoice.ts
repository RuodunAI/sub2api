// The scopes of one scoped action as the permission editors show them
// (CONTRACTS §21): every row, the caller's own, the caller's and its team's
// (CONTRACTS §103.2), chosen groups, chosen rows, rows within a chosen
// group, chosen plugin scopes; scopes the editors do not offer (group/*,
// group/*/account/X) are kept as they are. Pure;
// PermissionTree / ScopePicker render it.
import { ALL_SCOPE, OWN_SCOPE, SCOPABLE, TEAM_SCOPE, WILDCARD, parsePermission, patternKey, reduceScopes, refScope, scopeImplies, takesRowIds, takesTeam, type Scope } from './implies'

export interface ScopeChoice {
  all: boolean
  own: boolean
  team: boolean
  /** group/<id> */
  groups: number[]
  /** account/<id> or proxy/<id> */
  ids: number[]
  /** group/<g>/account/<id> as [g, id] */
  pairs: Array<[number, number]>
  /** Plugin scopes "<resource>/<value>". */
  refs: string[]
  /** Anything else held (group/*, group/X/account/*...): shown, removable, not offered. */
  other: Scope[]
}

const scope = (group: number, id: number): Scope => ({ all: false, own: false, group, id })

/** The choice the scopes make. */
export function choiceOf(scopes: readonly Scope[]): ScopeChoice {
  const c: ScopeChoice = { all: false, own: false, team: false, groups: [], ids: [], pairs: [], refs: [], other: [] }
  for (const s of scopes) {
    if (s.all) c.all = true
    else if (s.own) c.own = true
    else if (s.team) c.team = true
    else if (s.ref) c.refs.push(s.ref)
    else if (s.group === WILDCARD) c.other.push(s)
    else if (s.group && s.id) c.pairs.push([s.group, s.id])
    else if (s.group) c.groups.push(s.group)
    else c.ids.push(s.id)
  }
  return c
}

/** The scopes of a choice, without those another implies (every row: only that). */
export function scopesOf(c: ScopeChoice): Scope[] {
  if (c.all) return [ALL_SCOPE]
  return reduceScopes([
    ...(c.own ? [OWN_SCOPE] : []),
    ...(c.team ? [TEAM_SCOPE] : []),
    ...c.groups.map((g) => scope(g, 0)),
    ...c.ids.map((id) => scope(0, id)),
    ...c.pairs.map(([g, id]) => scope(g, id)),
    ...c.refs.map(refScope),
    ...c.other
  ])
}

/** keys with the keys of action replaced by its scopes, canonical (none: the action is not granted). */
export function withScopes(keys: ReadonlySet<string>, action: string, scopes: readonly Scope[]): Set<string> {
  const next = new Set([...keys].filter((k) => parsePermission(k)?.action !== action))
  for (const s of scopes) next.add(patternKey({ action, scope: s }))
  return next
}

/** What the editor may offer from a list: every id ('any'), these ids, or none (null). */
export type Offer<T = number> = 'any' | T[] | null

/** Plugin scopes "<resource>/<value>" the editor may offer. */
export type RefOffer = Offer<string>

/**
 * The options a caller holding `limit` (its scopes of the action) may grant:
 * only what its scopes imply (the server refuses the rest, details.missing).
 * Pairs are offered by group (any account within it); a held pair alone
 * offers nothing. account:create names no rows; a user action takes every
 * user or the team only; a plugin action no team.
 */
export interface ScopeOffer {
  all: boolean
  own: boolean
  team: boolean
  /** group/<id>, and the group of group/<id>/account/<x> (the account: any within it). */
  groups: Offer
  /** account/<id> or proxy/<id>. */
  ids: Offer
  /** Plugin scopes "<resource>/<value>". */
  refs: RefOffer
}

export function offerOf(limit: readonly Scope[], action = ''): ScopeOffer {
  const implied = (s: Scope) => limit.some((l) => scopeImplies(l, s))
  const all = implied(ALL_SCOPE)
  const anyGroup = implied(scope(WILDCARD, 0))
  const groups = limit.filter((l) => l.group > 0 && !l.id).map((l) => l.group)
  const ids = limit.filter((l) => l.id && !l.group).map((l) => l.id)
  const refs = limit.flatMap((l) => (l.ref ? [l.ref] : []))
  const listed = <T>(ok: boolean, list: T[]): Offer<T> => (ok ? 'any' : list.length ? list : null)
  const rows = SCOPABLE[action] !== 'user'
  return {
    all,
    own: rows && implied(OWN_SCOPE),
    team: takesTeam(action) && implied(TEAM_SCOPE),
    groups: listed(anyGroup, groups),
    ids: rows && takesRowIds(action) ? listed(all, ids) : null,
    refs: listed(all, refs)
  }
}

/** The scopes ticking the action grants at first: the widest offered (every row, else the team's, else the own rows), else none to choose from yet. */
export function defaultScopes(offer: ScopeOffer): Scope[] {
  if (offer.all) return [ALL_SCOPE]
  if (offer.team) return [TEAM_SCOPE]
  return offer.own ? [OWN_SCOPE] : []
}

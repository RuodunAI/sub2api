// Copying another role's grants into the role being edited, before it is
// saved: the role editor's "copy from role". Union adds what the source
// grants and the role does not; intersect keeps only what both grant. Both
// work per action on scopes, so "account:read:team" meets "account:read:own"
// as scopes, not as unrelated strings. Pure.
import { WILDCARD, parsePermission, patternKey, permissionIndex, scopeImplies, type Scope } from './implies'
import { actionOf, cellOfScopes, scopesOfCell } from './pluginGrants'

export type CopyMode = 'union' | 'intersect'

/** What the editor's catalog and grants say about a permission. */
export interface CopyRules {
  /** The editor may grant key (useEditorGrants.mayGrant). */
  mayGrant: (key: string) => boolean
  /** The action is removed or unknown to the catalog: it cannot be granted, and its scopes are not narrowed. */
  gone: (action: string) => boolean
  /** The action is critical: never ticked in bulk, only one by one (PermissionTree). */
  critical: (action: string) => boolean
}

export interface CopyResult {
  /** The role's keys after the copy: canonical, sorted. */
  keys: string[]
  /** Keys of the source a union left out, canonical and sorted: gone, beyond the editor, or critical ones it may grant (tick them by hand). */
  notGrantable: string[]
  critical: string[]
  gone: string[]
}

/** A group range without a row, or a row in no particular group: the two halves of "group/<g>/account/<id>". */
const plain = (s: Scope) => !s.all && !s.own && !s.team && !s.ref
const groupOnly = (s: Scope) => plain(s) && s.group !== 0 && s.id === 0
const rowAnyGroup = (s: Scope) => plain(s) && s.id !== 0 && (s.group === 0 || s.group === WILDCARD)

/**
 * The rows both scopes cover, as one scope, or null when that is no scope
 * (own within a group, two different groups or rows). One implying the other
 * is the narrower; a group range and an account in any group (account/X or
 * a wildcard group with that account) are that account within the group.
 */
function meet(a: Scope, b: Scope): Scope | null {
  if (scopeImplies(a, b)) return b
  if (scopeImplies(b, a)) return a
  const [g, r] = groupOnly(a) && rowAnyGroup(b) ? [a, b] : groupOnly(b) && rowAnyGroup(a) ? [b, a] : []
  return g && r ? { all: false, own: false, group: g.group, id: r.id } : null
}

const meets = (cur: readonly Scope[], src: readonly Scope[]) =>
  cur.flatMap((c) => src.map((s) => meet(c, s))).filter((s): s is Scope => !!s)

/**
 * current after copying source. union: every scope of source that current
 * does not already imply is added when the editor may grant it, the action is
 * neither critical nor gone; the rest of current stays (a narrower scope gives
 * way to a wider one). intersect: per action, the rows both sides cover, at
 * the narrower scope (current "account:read" and source "account:read:own"
 * keep "account:read:own"); a gone action keeps only its scopes of current the
 * source implies, since the server would not store a narrowed gone key.
 * Intersect only takes away, so it needs no grant. A key of current that does
 * not parse is kept as it is; one of source is ignored.
 */
export function copyGrants(current: readonly string[], source: readonly string[], mode: CopyMode, rules: CopyRules): CopyResult {
  const have = permissionIndex(current)
  const out: CopyResult = { keys: current.filter((k) => !parsePermission(k)), notGrantable: [], critical: [], gone: [] }
  for (const action of new Set([...current, ...source].map(actionOf))) {
    const cur = scopesOfCell(current, action)
    const src = scopesOfCell(source, action)
    if (mode === 'intersect') {
      const kept = rules.gone(action) ? cur.filter((c) => src.some((s) => scopeImplies(s, c))) : meets(cur, src)
      out.keys.push(...cellOfScopes(action, kept))
      continue
    }
    const added: Scope[] = []
    for (const scope of src) {
      const key = patternKey({ action, scope })
      if (have.has(key)) continue
      const left = rules.gone(action) ? out.gone : !rules.mayGrant(key) ? out.notGrantable : rules.critical(action) ? out.critical : null
      if (left) left.push(key)
      else added.push(scope)
    }
    out.keys.push(...cellOfScopes(action, [...cur, ...added]))
  }
  const sorted = (list: string[]) => [...new Set(list)].sort()
  return { keys: sorted(out.keys), notGrantable: sorted(out.notGrantable), critical: sorted(out.critical), gone: sorted(out.gone) }
}

// Copying another role's grants into the role being edited, before it is
// saved: the role editor's "copy from role". Union adds what the source
// grants and the role does not; intersect keeps only what both grant. Both
// work per action on scopes, so "account:read:team" meets "account:read:own"
// as scopes, not as unrelated strings. Pure.
import { parsePermission, permissionIndex, scopeImplies, type Scope } from './implies'
import { actionOf, cellOfScopes, scopesOfCell } from './pluginGrants'

export type CopyMode = 'union' | 'intersect'

export interface CopyResult {
  /** The role's keys after the copy: canonical, sorted. */
  keys: string[]
  /** Keys of source the role lacked that the editor may not grant, canonical: left out (the server would refuse the save). */
  skipped: string[]
}

/** The scopes of list that one of other implies. */
const coveredBy = (list: readonly Scope[], other: readonly Scope[]) => list.filter((s) => other.some((o) => scopeImplies(o, s)))

/**
 * current after copying source. union: every scope of source that current
 * does not already imply and that mayGrant allows is added, and the rest of
 * current stays. intersect: per action, the scopes one side holds that the
 * other implies (current "account:read" and source "account:read:own" keep
 * "account:read:own"); it only takes away, so it needs no grant. A key of
 * current that does not parse is kept as it is; one of source is ignored.
 */
export function copyGrants(current: readonly string[], source: readonly string[], mode: CopyMode, mayGrant: (key: string) => boolean): CopyResult {
  const have = permissionIndex(current)
  const skipped: string[] = []
  const keys = current.filter((k) => !parsePermission(k))
  for (const action of new Set([...current, ...source].map(actionOf))) {
    const cur = scopesOfCell(current, action)
    const src = scopesOfCell(source, action)
    if (mode === 'intersect') {
      keys.push(...cellOfScopes(action, [...coveredBy(cur, src), ...coveredBy(src, cur)]))
      continue
    }
    const added: Scope[] = []
    for (const s of src) {
      const [key] = cellOfScopes(action, [s])
      if (have.has(key)) continue
      if (mayGrant(key)) added.push(s)
      else skipped.push(key)
    }
    keys.push(...cellOfScopes(action, [...cur, ...added]))
  }
  return { keys: [...new Set(keys)].sort(), skipped: skipped.sort() }
}

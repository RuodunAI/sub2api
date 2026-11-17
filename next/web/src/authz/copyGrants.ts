// Copying another role's grants into the role being edited, before it is
// saved: the role editor's "copy from role". Merge keeps the role's ticks and
// adds the source's; overwrite ticks exactly what the source has. Both work
// per action on scopes, so "account:read:team" meets "account:read:own" as
// scopes, not as unrelated strings. Pure.
import { parsePermission, patternKey, permissionIndex, type Scope } from './implies'
import { actionOf, cellOfScopes, scopesOfCell } from './pluginGrants'

export type CopyMode = 'merge' | 'overwrite'

/** What the editor's catalog and grants say about a permission. */
export interface CopyRules {
  /** The editor may grant key (useEditorGrants.mayGrant). */
  mayGrant: (key: string) => boolean
  /** The action is removed or unknown to the catalog: it cannot be ticked here, so the role's keys of it stay as they are. */
  gone: (action: string) => boolean
  /** The action is critical: never ticked in bulk, only one by one (PermissionTree). */
  critical: (action: string) => boolean
}

export interface CopyResult {
  /** The role's keys after the copy: canonical, sorted. */
  keys: string[]
  /** Keys of the source left out, canonical and sorted: gone, beyond the editor, or critical ones it may grant (tick them by hand). */
  notGrantable: string[]
  critical: string[]
  gone: string[]
}

/**
 * current after copying source. A scope of source that current implies is
 * taken as it is (no grant needed: overwrite may narrow "account:read" to
 * "account:read:own"); any other is taken when the action is not gone, the
 * editor may grant it and it is not critical, and named otherwise. merge keeps
 * every scope of current as well (a narrower one gives way to a wider one);
 * overwrite keeps none of them, except a gone action's, which the editor can
 * neither see nor tick. A key of current that does not parse is kept as it
 * is; one of source is ignored.
 */
export function copyGrants(current: readonly string[], source: readonly string[], mode: CopyMode, rules: CopyRules): CopyResult {
  const have = permissionIndex(current)
  const out: CopyResult = { keys: current.filter((k) => !parsePermission(k)), notGrantable: [], critical: [], gone: [] }
  for (const action of new Set([...current, ...source].map(actionOf))) {
    const gone = rules.gone(action)
    const taken: Scope[] = mode === 'merge' || gone ? scopesOfCell(current, action) : []
    for (const scope of scopesOfCell(source, action)) {
      const key = patternKey({ action, scope })
      const left = have.has(key) ? null : gone ? out.gone : !rules.mayGrant(key) ? out.notGrantable : rules.critical(action) ? out.critical : null
      if (left) left.push(key)
      else if (!gone) taken.push(scope)
    }
    out.keys.push(...cellOfScopes(action, taken))
  }
  const sorted = (list: string[]) => [...new Set(list)].sort()
  return { keys: sorted(out.keys), notGrantable: sorted(out.notGrantable), critical: sorted(out.critical), gone: sorted(out.gone) }
}

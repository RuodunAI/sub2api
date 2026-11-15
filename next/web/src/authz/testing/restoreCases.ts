// The cases of "restore the plugin's suggestion" that the role editor and the
// permission matrix must answer alike (pluginGrants restoreCell /
// suggestionState): each spec runs them through its own entry. One scoped
// action of plugin m; `editor` is what the editor holds, `cell` the role's
// keys of the action, `want` the suggestion; `restored` is the cell after a
// restore (unchanged when out of reach).
import type { SuggestionState } from '../pluginGrants'

export const RESTORE_ACTION = 'plugin.m:flow:manage'
const A = RESTORE_ACTION

export interface RestoreCase {
  name: string
  editor: string[]
  cell: string[]
  want: string[]
  state: SuggestionState
  restored: string[]
}

export const RESTORE_CASES: RestoreCase[] = [
  { name: 'the editor holds part of the scopes the suggestion grants', editor: [`${A}:own`], cell: [`${A}:own`], want: [A], state: 'unreachable', restored: [`${A}:own`] },
  { name: 'the editor does not hold the action', editor: [], cell: [], want: [A], state: 'unreachable', restored: [] },
  { name: 'taking away needs nothing held', editor: [], cell: [`${A}:own`], want: [], state: 'differs', restored: [] },
  { name: 'the suggestion is narrower than the grant', editor: [A], cell: [`${A}:own`, `${A}:flow/3`], want: [`${A}:own`], state: 'differs', restored: [`${A}:own`] },
  { name: 'the editor holds what the suggestion grants', editor: [`${A}:own`], cell: [], want: [`${A}:own`], state: 'differs', restored: [`${A}:own`] },
  { name: 'as suggested', editor: [`${A}:own`], cell: [`${A}:own`], want: [`${A}:own`], state: 'same', restored: [`${A}:own`] }
]

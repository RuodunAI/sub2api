// How a plugin's permissions are laid out, shared by the permission matrix
// (consent page, plugin detail) and the role editor's plugin modules, so the
// two entries to the same grants show them alike.
import type { LText, PermissionGroup } from '@/api/types'
import { lt } from '@/i18n'

export interface PermSection<P> {
  /** '' for the permissions of no listed group. */
  key: string
  label: string
  perms: P[]
}

/** perms under their groups, in the groups' order; those of no listed group last, without a heading. Empty sections are left out. */
export function groupSections<P extends { group?: string }>(perms: readonly P[], groups: readonly PermissionGroup[] = []): PermSection<P>[] {
  const known = new Set(groups.map((g) => g.key))
  const out = groups.map((g) => ({ key: g.key, label: lt(g.label) || g.key, perms: perms.filter((p) => p.group === g.key) }))
  out.push({ key: '', label: '', perms: perms.filter((p) => !p.group || !known.has(p.group)) })
  return out.filter((s) => s.perms.length)
}

/** The hover hint of a permission: its key, then what it allows. */
export const permHint = (key: string, description?: LText) => [key, description && lt(description)].filter(Boolean).join('\n')

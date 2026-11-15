import { isApiError } from '@sub2api/host'

/**
 * What a refused role write lacked (403 details: missing permissions, missing
 * managed roles, "all custom roles"), as one line; prefixed with the role it
 * was refused for when the refusal names one (details.role: the plugin matrix
 * writes several roles at once). '' for any other error. Shared by the role
 * editor and the plugin permission matrix.
 */
export function missingText(e: unknown, t: (key: string, values?: Record<string, unknown>) => string): string {
  if (!isApiError(e)) return ''
  const d = e.details
  const list = (v: unknown) => (Array.isArray(v) ? v.join(t('roles.listSep')) : '')
  const text = [
    list(d.missing) && t('roles.missingPermissions', { keys: list(d.missing) }),
    list(d.missing_assignable) && t('roles.missingAssignable', { keys: list(d.missing_assignable) }),
    d.missing_assign_custom ? t('roles.missingAssignCustom') : ''
  ]
    .filter(Boolean)
    .join(' · ')
  return text && typeof d.role === 'string' && d.role ? t('roles.missingFor', { role: d.role, text }) : text
}

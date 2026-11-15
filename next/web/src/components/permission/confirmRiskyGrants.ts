import { confirm } from '@sub2api/ui'
import type { LText } from '@/api/types'
import { i18n, lt } from '@/i18n'
import type { RiskyGrant } from '@/authz/pluginGrants'

/**
 * Before a save that grants sensitive or critical permissions
 * (pluginGrants.newRiskyGrants), lists them, one line per permission with
 * the roles it goes to, and asks to go on: a group ticks no critical row
 * (D1), but rows ticked one by one, a restore or a merge still may. The role
 * editor and the plugin detail matrix ask alike. Resolves true when there is
 * nothing to ask or the user goes on.
 */
export async function confirmRiskyGrants(grants: readonly RiskyGrant[], labelOf: (action: string) => LText | undefined, roleName: (role: string) => string): Promise<boolean> {
  if (!grants.length) return true
  const { t } = i18n.global
  const byAction = new Map<string, RiskyGrant[]>()
  for (const g of grants) byAction.set(g.action, [...(byAction.get(g.action) ?? []), g])
  const lines = [...byAction.values()].map((list) =>
    t('plugins.matrix.confirmRiskyLine', {
      perm: lt(labelOf(list[0].action)) || list[0].action,
      risk: t(`roles.risk.${list[0].risk}`),
      roles: list.map((g) => roleName(g.role)).join(t('roles.listSep'))
    })
  )
  return confirm({
    title: t('plugins.matrix.confirmRiskyTitle'),
    message: [t('plugins.matrix.confirmRisky'), ...lines].join('\n'),
    confirmText: t('common.save'),
    danger: grants.some((g) => g.risk === 'critical')
  })
}

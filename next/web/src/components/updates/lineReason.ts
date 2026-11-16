import { useI18n } from 'vue-i18n'
import type { LineCheck } from '@/stores/updates'

/**
 * The reason of a line's check in the console's language. The gateway classes
 * it by reason_code; an incompatible release keeps its findings (protocol and
 * platform checks) in their original words after the localized lead.
 */
export function useLineReason() {
  const { t } = useI18n()
  return (check?: LineCheck | null): string => {
    switch (check?.reason_code) {
      case 'no_release':
      case 'version_conflict':
        return t(`coreUpdates.reasons.${check.reason_code}`)
      case 'incompatible':
        return check.reason ? t('coreUpdates.reasons.incompatible', { details: check.reason }) : t('coreUpdates.incompatible')
    }
    return check?.reason || ''
  }
}

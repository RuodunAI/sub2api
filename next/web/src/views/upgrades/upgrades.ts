import { useOptionalT } from '@/composables/optionalText'
import type { UpdateIssue } from '@/stores/updates'

// Shapes and labels shared by the upgrade page, its step lanes and the version badge.

export interface UpgradeStep { step_id: number; node_id: string; action: string; status: string; error?: string }
export interface UpgradeEvent { id: number; kind: string; message: string; created_at: string }

/** `label(group, value)`: upgrades.<group>.<value> when translated, else the raw value. */
export function useUpgradeLabel() {
  const tr = useOptionalT()
  return (group: string, value: string) => tr(`upgrades.${group}.${value}`, undefined, value)
}

/**
 * Text of a preflight blocker or an apply confirmation reason:
 * upgrades.issues.<code> with its params, else the gateway's English message.
 * The message is a param too, for codes whose detail only it names (which
 * plugin, which compatibility check).
 */
export function useIssueText() {
  const tr = useOptionalT()
  return (issue: UpdateIssue) => tr(`upgrades.issues.${issue.code}`, { ...issue.params, message: issue.message }, issue.message)
}

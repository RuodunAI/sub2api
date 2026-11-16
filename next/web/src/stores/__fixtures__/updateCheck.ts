import type { UpdateCheck } from '../updates'

// A successful check of both lines, nothing newer; tests override fields.
const line = { latest_version: '0.1.291', has_update: false, compatible: true, checked_at: '2026-11-16T08:00:00Z', last_success_at: '2026-11-16T08:00:00Z' }

export function updateCheck(over: { core?: object; gateway?: object; enabled?: boolean } = {}): UpdateCheck {
  return {
    enabled: over.enabled ?? true,
    source: { kind: 'github', repository: 'Sub2API-Devs/sup2api', url: '' },
    core: { ...line, current_version: '0.1.290', ...over.core },
    gateway: { ...line, nodes: [{ node_id: 'n1', version: '0.1.135' }], self_update: true, ...over.gateway }
  }
}

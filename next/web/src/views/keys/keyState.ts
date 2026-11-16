import type { ApiKey } from '@/api/types'
import type { Tone } from '@sub2api/ui'
import { quotaOf } from './keyLimits'

/** What a key's status badge shows: its own switch, then expiry, then a used-up quota. */
export const KEY_STATES = ['active', 'disabled', 'expired', 'exhausted'] as const
export type KeyState = (typeof KEY_STATES)[number]

/** Past its expiry, from the instant of expiry on (as the gateway rejects it). */
export const isExpired = (k: ApiKey) => !!k.expires_at && new Date(k.expires_at).getTime() <= Date.now()

export function keyState(k: ApiKey): KeyState {
  if (k.status !== 'active') return 'disabled'
  if (isExpired(k)) return 'expired'
  return quotaOf(k).exhausted ? 'exhausted' : 'active'
}

/** Badge tones the generic status tones lack. */
export const KEY_STATE_TONES: Record<string, Tone> = { exhausted: 'danger' }

/** The status filter's choices (the server's `status` query values); it has none for a used-up quota. */
export const KEY_STATE_FILTERS = ['active', 'disabled', 'expired'] as const satisfies readonly KeyState[]

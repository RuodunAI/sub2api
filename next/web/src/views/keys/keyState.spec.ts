import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ApiKey } from '@/api/types'
import { isExpired, keyState } from './keyState'

const NOW = new Date('2026-11-10T12:00:00.000Z')
const at = (offsetMs: number) => new Date(NOW.getTime() + offsetMs).toISOString()
const key = (k: Partial<ApiKey> = {}) =>
  ({ id: 1, name: 'k', group_id: 1, key_prefix: 'sk', status: 'active', created_at: '', ...k }) as ApiKey
const PAST = at(-60_000)
const SPENT = { quota_limit: '5', quota_used: '5' }

describe('API key state', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
  })
  afterEach(() => vi.useRealTimers())

  it('is active for an enabled key that neither expired nor used up its quota', () => {
    expect(keyState(key())).toBe('active')
    expect(keyState(key({ expires_at: at(60_000), quota_limit: '5', quota_used: '4.99' }))).toBe('active')
  })

  it('ranks the key being switched off over its expiry, and expiry over a used-up quota', () => {
    expect(keyState(key({ status: 'disabled', expires_at: PAST, ...SPENT }))).toBe('disabled')
    expect(keyState(key({ status: 'disabled' }))).toBe('disabled')
    expect(keyState(key({ expires_at: PAST, ...SPENT }))).toBe('expired')
    expect(keyState(key({ expires_at: PAST }))).toBe('expired')
    expect(keyState(key({ expires_at: at(60_000), ...SPENT }))).toBe('exhausted')
    expect(keyState(key(SPENT))).toBe('exhausted')
  })

  it('shows a status this console does not know as disabled: the key is not usable', () => {
    expect(keyState(key({ status: 'suspended' }))).toBe('disabled')
  })

  it('expires a key at the instant of its expiry, never one without an expiry', () => {
    expect(isExpired(key({ expires_at: at(1) }))).toBe(false)
    expect(isExpired(key({ expires_at: at(0) }))).toBe(true)
    expect(isExpired(key({ expires_at: at(-1) }))).toBe(true)
    expect(isExpired(key({ expires_at: null }))).toBe(false)
    expect(isExpired(key())).toBe(false)
  })

  it('uses a quota up when the charges reach or pass the limit; no limit is never used up', () => {
    expect(keyState(key({ quota_limit: '5', quota_used: '5.000001' }))).toBe('exhausted')
    expect(keyState(key({ quota_limit: '5', quota_used: '4.999999' }))).toBe('active')
    expect(keyState(key({ quota_limit: '0', quota_used: '120' }))).toBe('active')
    expect(keyState(key({ quota_used: '120' }))).toBe('active')
  })
})

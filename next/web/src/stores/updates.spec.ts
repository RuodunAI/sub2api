import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { lineStatus, useUpdatesStore, type UpdateCheck } from './updates'
import { updateCheck } from './__fixtures__/updateCheck'

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@sub2api/host', async orig => ({ ...(await orig<object>()), api }))

beforeEach(() => {
  setActivePinia(createPinia())
  api.get.mockReset()
  api.post.mockReset()
})

describe('lineStatus', () => {
  it('never reports a failed or never successful check as up to date', () => {
    expect(lineStatus(updateCheck({ core: { error: 'rate limited' } }), 'core')).toBe('failed')
    expect(lineStatus(updateCheck({ core: { latest_version: '' } }), 'core')).toBe('failed')
    expect(lineStatus(updateCheck(), 'core', true)).toBe('failed')
    expect(lineStatus(updateCheck(), 'core')).toBe('upToDate')
  })

  it('tells unchecked, disabled, available and blocked lines apart', () => {
    expect(lineStatus(null, 'core')).toBe('unchecked')
    expect(lineStatus(updateCheck({ gateway: { checked_at: '', latest_version: '' } }), 'gateway')).toBe('unchecked')
    expect(lineStatus(updateCheck({ enabled: false }), 'gateway')).toBe('disabled')
    expect(lineStatus(updateCheck({ gateway: { has_update: true } }), 'gateway')).toBe('available')
    expect(lineStatus(updateCheck({ core: { compatible: false, reason: 'schema' } }), 'core')).toBe('blocked')
  })

  it('reports a successful check that found no stable release by its reason, not as failed', () => {
    const none = { latest_version: '', compatible: false, reason: 'the update source has no stable gateway release' }
    expect(lineStatus(updateCheck({ gateway: none }), 'gateway')).toBe('noRelease')
    expect(lineStatus(updateCheck({ gateway: { ...none, error: 'timeout' } }), 'gateway')).toBe('failed')
  })
})

describe('updates store', () => {
  it('flags an update of either line, not while the check request fails', async () => {
    const store = useUpdatesStore()
    api.get.mockResolvedValueOnce(updateCheck({ gateway: { has_update: true } }))
    await store.check(true)
    expect(api.get).toHaveBeenCalledWith('/system/update-check?force=true')
    expect(store.available).toBe(true)

    api.get.mockRejectedValueOnce(new Error('down'))
    await store.check()
    expect(store.error).toBe('down')
    expect(store.available).toBe(false)
  })

  it('drops a check that answers after invalidate', async () => {
    const store = useUpdatesStore()
    let answer!: (v: UpdateCheck) => void
    api.get.mockReturnValueOnce(new Promise(r => (answer = r)))
    const pending = store.check()
    store.invalidate()
    answer(updateCheck({ core: { has_update: true } }))
    await pending
    expect(store.result).toBeNull()
    expect(store.busy).toBe(false)
  })

  it('applies a line version, confirmed only when asked', async () => {
    const store = useUpdatesStore()
    api.post.mockResolvedValue({ status: 'started', plan_id: 'p1' })
    await store.apply('gateway', '0.1.136')
    await store.apply('core', '0.1.291', true)
    expect(api.post.mock.calls).toEqual([
      ['/system/updates/apply', { line: 'gateway', version: '0.1.136', confirm: false }],
      ['/system/updates/apply', { line: 'core', version: '0.1.291', confirm: true }]
    ])
  })
})

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { session } from '@sub2api/host'
import { i18n } from '@/i18n'
import SessionsCard from './SessionsCard.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), del: vi.fn() }))
const ui = vi.hoisted(() => ({ confirm: vi.fn(), toast: vi.fn() }))
vi.mock('@sub2api/host', async (orig) => ({ ...(await orig<object>()), api }))
vi.mock('@sub2api/ui', async (orig) => ({ ...(await orig<object>()), confirm: ui.confirm, toast: ui.toast }))
enableAutoUnmount(afterEach)

const row = (id: string, current: boolean) => ({ id, method: 'password', ip: '198.51.100.9', user_agent: 'Firefox/1', created_at: '2026-11-16T00:00:00Z', last_used_at: '2026-11-16T01:00:00Z', current })
const render = () => mount(SessionsCard, { global: { plugins: [i18n], stubs: { teleport: true } }, attachTo: document.body })
const revokeOthers = (w: ReturnType<typeof render>) => w.get('[data-testid="sessions-revoke-others"]')

describe('sessions card', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'zh'
    for (const f of [api.get, api.post, api.del, ui.confirm, ui.toast]) f.mockReset()
    ui.confirm.mockResolvedValue(true)
    session.fromTokenResponse({ access_token: 's2s_a' })
  })
  afterEach(() => session.clear())

  it('ends the other sessions even with none listed, and takes up the new token of this one', async () => {
    api.get.mockResolvedValue([row('mine', true)])
    api.post.mockResolvedValue({ access_token: 's2s_b' })
    const w = render()
    await flushPromises()
    expect(w.text()).toContain('198.51.100.9')
    expect(revokeOthers(w).attributes('disabled')).toBeUndefined()
    await revokeOthers(w).trigger('click')
    await flushPromises()
    expect(api.post).toHaveBeenCalledWith('/me/sessions/revoke-others', {})
    expect(session.get()?.access_token).toBe('s2s_b')
    expect(api.get).toHaveBeenCalledTimes(2)
  })

  it('keeps a newer token another tab stored meanwhile, and copes with no new token', async () => {
    api.get.mockResolvedValue([row('mine', true), row('other', false)])
    api.post.mockImplementation(async () => {
      session.fromTokenResponse({ access_token: 's2s_other' })
      return { access_token: 's2s_late' }
    })
    const w = render()
    await flushPromises()
    await revokeOthers(w).trigger('click')
    await flushPromises()
    expect(session.get()?.access_token).toBe('s2s_other')

    api.post.mockReset().mockResolvedValue(null)
    await revokeOthers(w).trigger('click')
    await flushPromises()
    expect(session.get()?.access_token).toBe('s2s_other')
  })

  it('offers nothing to end without the current session listed', async () => {
    api.get.mockResolvedValue([row('other', false)])
    const w = render()
    await flushPromises()
    expect(revokeOthers(w).attributes('disabled')).toBeDefined()
  })
})

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { ApiError } from '@sub2api/host'
import { i18n } from '@/i18n'
import { updateCheck } from '@/stores/__fixtures__/updateCheck'
import type { UpdateCheck } from '@/stores/updates'
import VersionBadge from './VersionBadge.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
const ui = vi.hoisted(() => ({ confirm: vi.fn() }))
const router = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('@sub2api/host', async orig => ({ ...(await orig<object>()), api }))
vi.mock('@sub2api/ui', async orig => ({ ...(await orig<object>()), confirm: ui.confirm }))
vi.mock('vue-router', () => ({ useRouter: () => router }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ has: () => true }) }))
enableAutoUnmount(afterEach)

let check: UpdateCheck
beforeEach(() => {
  setActivePinia(createPinia())
  i18n.global.locale.value = 'zh'
  check = updateCheck()
  api.get.mockReset().mockImplementation((path: string) => Promise.resolve(path === '/system/version' ? { version: '0.1.290', managed: true } : check))
  api.post.mockReset()
  ui.confirm.mockReset()
  router.push.mockReset()
})

async function open() {
  const w = mount(VersionBadge, { global: { plugins: [i18n], stubs: { teleport: true } }, attachTo: document.body })
  await flushPromises()
  await w.get('[data-testid="version-badge"]').trigger('click')
  await flushPromises()
  return w
}
const section = (w: Awaited<ReturnType<typeof open>>, line: string) => w.get(`[data-testid="update-line-${line}"]`)
const dot = (w: Awaited<ReturnType<typeof open>>) => w.find('[data-testid="version-badge-dot"]').exists()

describe('version badge', () => {
  it('shows both lines, with a dot when either has an update', async () => {
    let w = await open()
    expect(section(w, 'core').text()).toContain('已是最新')
    expect(section(w, 'gateway').text()).toContain('0.1.135')
    expect(dot(w)).toBe(false)
    w.unmount()
    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136' } })
    setActivePinia(createPinia())
    w = await open()
    expect(dot(w)).toBe(true)
    expect(section(w, 'gateway').text()).toContain('发现新版本')
  })

  it('says a failed check failed, never up to date', async () => {
    check = updateCheck({ core: { error: 'GitHub API rate limited' }, gateway: { latest_version: '', last_success_at: '' } })
    const w = await open()
    for (const line of ['core', 'gateway']) {
      expect(section(w, line).get('[data-testid="update-line-status"]').text()).toBe('检测失败')
      expect(section(w, line).text()).not.toContain('已是最新')
    }
    expect(section(w, 'core').text()).toContain('GitHub API rate limited')
  })

  it('confirms the reasons, resends with confirm and opens the started plan', async () => {
    check = updateCheck({ core: { has_update: true, latest_version: '0.1.291' } })
    api.post.mockResolvedValueOnce({ status: 'needs_confirmation', reasons: ['primary-first stops the service'] }).mockResolvedValueOnce({ status: 'started', plan_id: 'plan-9' })
    ui.confirm.mockResolvedValue(true)
    const w = await open()
    await section(w, 'core').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(ui.confirm.mock.calls[0][0].message).toContain('primary-first stops the service')
    expect(api.post.mock.calls.map(c => c[1])).toEqual([
      { line: 'core', version: '0.1.291', confirm: false },
      { line: 'core', version: '0.1.291', confirm: true }
    ])
    expect(router.push).toHaveBeenCalledWith({ path: '/system/upgrades', query: { upgrade: 'plan-9' } })
  })

  it('stops when the confirmation is declined', async () => {
    check = updateCheck({ core: { has_update: true } })
    api.post.mockResolvedValueOnce({ status: 'needs_confirmation', reasons: ['single node'] })
    ui.confirm.mockResolvedValue(false)
    const w = await open()
    await section(w, 'core').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(api.post).toHaveBeenCalledTimes(1)
    expect(router.push).not.toHaveBeenCalled()
  })

  it('offers the gateway image instead of a one-click update the gateway cannot run', async () => {
    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136', self_update: false, image: 'ghcr.io/o/sup2api-gateway@sha256:abc' } })
    let w = await open()
    expect(section(w, 'gateway').find('[data-testid="update-apply"]').exists()).toBe(false)
    expect(section(w, 'gateway').text()).toContain('ghcr.io/o/sup2api-gateway@sha256:abc')
    expect(section(w, 'gateway').find('[data-testid="update-manual"]').exists()).toBe(true)
    w.unmount()

    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136', self_update: true } })
    api.post.mockRejectedValueOnce(new ApiError(501, { code: 'not_implemented', message: 'not implemented' }))
    setActivePinia(createPinia())
    w = await open()
    await section(w, 'gateway').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(section(w, 'gateway').find('[data-testid="update-apply"]').exists()).toBe(false)
    expect(section(w, 'gateway').find('[data-testid="update-manual"]').exists()).toBe(true)
  })
})

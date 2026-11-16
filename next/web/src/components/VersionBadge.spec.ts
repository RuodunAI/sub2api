import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { ApiError } from '@sub2api/host'
import { i18n } from '@/i18n'
import { updateCheck } from '@/stores/__fixtures__/updateCheck'
import type { UpdateCheck } from '@/stores/updates'
import VersionBadge from './VersionBadge.vue'

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
const ui = vi.hoisted(() => ({ confirm: vi.fn(), toast: vi.fn() }))
const router = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock('@sub2api/host', async orig => ({ ...(await orig<object>()), api }))
vi.mock('@sub2api/ui', async orig => ({ ...(await orig<object>()), confirm: ui.confirm, toast: ui.toast }))
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
  ui.toast.mockReset()
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

  it('confirms the reasons in Chinese, resends with confirm and opens the started plan', async () => {
    check = updateCheck({ core: { has_update: true, latest_version: '0.1.291' } })
    const reason_codes = [
      { code: 'schema_change', message: 'the release changes the database schema' },
      { code: 'too_few_serving', params: { min: '2' }, message: 'fewer than 2 nodes serve, so no node can take over another\'s traffic' }
    ]
    api.post.mockResolvedValueOnce({ status: 'needs_confirmation', reason_codes }).mockResolvedValueOnce({ status: 'started', plan_id: 'plan-9' })
    ui.confirm.mockResolvedValue(true)
    const w = await open()
    await section(w, 'core').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(ui.confirm.mock.calls[0][0].message).toBe('将核心更新到 0.1.291 前请确认：\n• 此版本改动数据库结构，不能滚动更新\n• 提供服务的节点少于 2 个，没有节点能接管其他节点的流量，不能滚动更新')
    expect(api.post.mock.calls.map(c => c[1])).toEqual([
      { line: 'core', version: '0.1.291', confirm: false },
      { line: 'core', version: '0.1.291', confirm: true }
    ])
    expect(router.push).toHaveBeenCalledWith({ path: '/system/upgrades', query: { upgrade: 'plan-9' } })
  })

  it('stops when the confirmation is declined', async () => {
    check = updateCheck({ core: { has_update: true } })
    api.post.mockResolvedValueOnce({ status: 'needs_confirmation', reason_codes: [{ code: 'single_node', params: { seconds: '10' }, message: 'single node' }] })
    ui.confirm.mockResolvedValue(false)
    const w = await open()
    await section(w, 'core').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(api.post).toHaveBeenCalledTimes(1)
    expect(router.push).not.toHaveBeenCalled()
  })

  it('words the blockers of a refused preflight', async () => {
    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136', self_update: true } })
    const blocker_codes = [{ code: 'node_not_ready', params: { node: 'n2' }, message: 'node not locally ready: n2' }, { code: 'upgrade_active', message: 'another upgrade is active' }]
    api.post.mockRejectedValueOnce(new ApiError(409, { code: 'preflight_blocked', message: 'node not locally ready: n2; another upgrade is active', details: { blocker_codes } }))
    const w = await open()
    await section(w, 'gateway').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(ui.toast).toHaveBeenCalledWith('预检未通过，无法更新：节点 n2 没有在本地提供服务 · 已有未完成的升级任务', 'error')
    expect(router.push).not.toHaveBeenCalled()
  })

  it('updates the gateway in one click, or offers its image when a node cannot update itself', async () => {
    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136', self_update: false, image: 'ghcr.io/o/sup2api-gateway@sha256:abc' } })
    let w = await open()
    expect(section(w, 'gateway').find('[data-testid="update-apply"]').exists()).toBe(false)
    expect(section(w, 'gateway').text()).toContain('ghcr.io/o/sup2api-gateway@sha256:abc')
    expect(section(w, 'gateway').get('[data-testid="update-manual"]').text()).toContain('部分节点的网关版本过旧')
    w.unmount()

    check = updateCheck({ gateway: { has_update: true, latest_version: '0.1.136', self_update: true } })
    api.post.mockResolvedValueOnce({ status: 'started', plan_id: 'plan-g' })
    setActivePinia(createPinia())
    w = await open()
    expect(section(w, 'gateway').find('[data-testid="update-manual"]').exists()).toBe(false)
    await section(w, 'gateway').get('[data-testid="update-apply"]').trigger('click')
    await flushPromises()
    expect(api.post.mock.calls[0][1]).toEqual({ line: 'gateway', version: '0.1.136', confirm: false })
    expect(router.push).toHaveBeenCalledWith({ path: '/system/upgrades', query: { upgrade: 'plan-g' } })
  })

  it('explains a line with no stable release instead of calling the check failed', async () => {
    check = updateCheck({ core: { latest_version: '', compatible: false, reason: 'the update source has no stable core release' } })
    const w = await open()
    expect(section(w, 'core').get('[data-testid="update-line-status"]').text()).toBe('没有稳定版本')
    expect(section(w, 'core').get('[data-testid="update-blocker"]').text()).toBe('the update source has no stable core release')
    expect(w.get('[data-testid="version-badge"]').attributes('title')).toContain('核心: 没有稳定版本')
  })

  it('lists the gateway versions oldest first and counts unreported nodes apart', async () => {
    const nodes = ['0.1.135', '', '0.1.99', '0.1.135'].map((version, i) => ({ node_id: `n${i}`, version }))
    check = updateCheck({ gateway: { nodes } })
    const w = await open()
    expect(section(w, 'gateway').text()).toContain('0.1.99, 0.1.135')
    expect(section(w, 'gateway').text()).not.toContain('未知')
    expect(section(w, 'gateway').get('[data-testid="update-unreported"]').text()).toContain('1/4 个节点')
  })
})

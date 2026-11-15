import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { i18n } from '@/i18n'
import PluginDetailView from './PluginDetailView.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn(), confirm: vi.fn(async () => false) }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: { get: mocks.get } }))
vi.mock('@sub2api/ui', async (original) => ({ ...await original<object>(), confirm: mocks.confirm }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ me: { roles: [] }, has: () => true }) }))
enableAutoUnmount(afterEach)

// The permissions tab reports unsaved edits as PermissionsTab does: emit('dirty', true).
const PermissionsTab = { name: 'PermissionsTab', emits: ['dirty'], template: '<button data-testid="perm-tab" @click="$emit(\'dirty\', true)" />' }

async function render(query = '') {
  mocks.get.mockImplementation(async (path: string) => (path === '/plugins/guard' ? { key: 'guard', name: 'Guard', status: 'enabled', grants: [], versions: [] } : null))
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/plugins/:key', component: PluginDetailView },
      { path: '/plugins', component: { template: '<div />' } }
    ]
  })
  await router.push('/plugins/guard' + query)
  const w = mount({ template: '<router-view />' }, { global: { plugins: [i18n, router], stubs: { PermissionsTab, OverviewTab: true, NodesTab: true } } })
  await flushPromises()
  return { w, router }
}
const tabButton = (w: Awaited<ReturnType<typeof render>>['w'], label: string) => w.findAll('[role="tab"]').find((b) => b.text().startsWith(label))!

describe('plugin detail view', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    i18n.global.locale.value = 'en'
  })

  it('?tab=grants opens the permissions tab', async () => {
    const { w, router } = await render('?tab=grants')
    expect(w.find('[data-testid="perm-tab"]').exists()).toBe(true)
    expect(router.currentRoute.value.query.tab).toBe('permissions')
  })

  it('switching tabs with unsaved matrix asks first', async () => {
    const { w, router } = await render('?tab=permissions')
    await w.get('[data-testid="perm-tab"]').trigger('click')

    // Staying keeps the tab and its edits.
    await tabButton(w, 'Nodes').trigger('click')
    await flushPromises()
    expect(mocks.confirm).toHaveBeenCalledTimes(1)
    expect(w.find('[data-testid="perm-tab"]').exists()).toBe(true)

    // So does leaving the page.
    await router.push('/plugins')
    await flushPromises()
    expect(mocks.confirm).toHaveBeenCalledTimes(2)
    expect(router.currentRoute.value.path).toBe('/plugins/guard')

    mocks.confirm.mockResolvedValueOnce(true)
    await tabButton(w, 'Nodes').trigger('click')
    await flushPromises()
    expect(w.find('[data-testid="perm-tab"]').exists()).toBe(false)
    expect(router.currentRoute.value.query.tab).toBe('nodes')
  })
})

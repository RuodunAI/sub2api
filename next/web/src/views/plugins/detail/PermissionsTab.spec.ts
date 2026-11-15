import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { ApiError } from '@sub2api/host'
import { i18n } from '@/i18n'
import type { PluginRoleMatrix } from '@/api/types'
import PermissionsTab from './PermissionsTab.vue'
import type { PluginDetail } from '../pluginUtil'

const mocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), toast: vi.fn(), refresh: vi.fn(async () => undefined) }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: { get: mocks.get, put: mocks.put } }))
vi.mock('@sub2api/ui', async (original) => ({ ...await original<object>(), toast: mocks.toast }))
const auth = vi.hoisted(() => ({ denied: new Set<string>(), roles: [] as string[] }))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    me: { roles: auth.roles },
    has: (k: string) => !auth.denied.has(k),
    hasAction: (a: string) => !auth.denied.has(a),
    scopes: () => [{ all: true }],
    refreshPermissions: mocks.refresh
  })
}))
enableAutoUnmount(afterEach)

const READ = 'plugin.guard:flow:read'
const EXPORT = 'plugin.guard:record:export'
const OLD = 'plugin.guard:alerts:mute'
const row = (key: string, name: string, over: object = {}) => ({ id: 0, key, name, builtin: true, superuser: false, editable: true, permission_keys: [] as string[], ...over })
const MATRIX: PluginRoleMatrix = {
  plugin_key: 'guard',
  module: {
    module: 'plugin.guard',
    label: 'Guard',
    source: 'plugin',
    plugin_key: 'guard',
    status: 'active',
    permissions: [
      { key: READ, label: 'Read flows', risk: 'low', status: 'active', defaults: { admin: [READ], operator: [READ] } },
      { key: EXPORT, label: 'Export records', risk: 'sensitive', status: 'active', defaults: { admin: [EXPORT] } },
      { key: OLD, label: 'Mute alerts', risk: 'low', status: 'removed' }
    ]
  },
  roles: [
    row('super_admin', 'Super admin', { superuser: true, editable: false, denied: 'superuser_role' }),
    row('admin', 'Admin', { permission_keys: [EXPORT, READ] }),
    row('operator', 'Operator', { permission_keys: [OLD, READ] }),
    row('lead', 'Lead', { builtin: false, editable: false, denied: 'not_assignable' })
  ]
}

async function render(detail: Partial<PluginDetail> = {}) {
  mocks.get.mockImplementation(async (path: string) => (path.startsWith('/permissions/plugins/') ? structuredClone(MATRIX) : []))
  const w = mount(PermissionsTab, { props: { detail: { key: 'guard', status: 'enabled', grants: [], ...detail } as unknown as PluginDetail }, global: { plugins: [i18n] } })
  await flushPromises()
  return w
}
type W = Awaited<ReturnType<typeof render>>
const box = (w: W, action: string, role: string) => w.get<HTMLInputElement>(`tr[data-permission="${action}"] td[data-role="${role}"] input[type="checkbox"]`)

describe('plugin permissions tab', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    auth.denied.clear()
    auth.roles = []
    i18n.global.locale.value = 'en'
  })

  it('shows host grants (GrantsTab) and the matrix', async () => {
    const w = await render()
    expect(mocks.get).toHaveBeenCalledWith('/plugins/guard/grants')
    expect(mocks.get).toHaveBeenCalledWith('/permissions/plugins/guard')
    expect(w.get('[data-testid="plugin-host-permissions"]').text()).toContain('Access the plugin requests')
    expect(box(w, READ, 'operator').element.checked).toBe(true)
    expect(box(w, EXPORT, 'operator').element.checked).toBe(false)
    expect(w.find('[data-testid="matrix-save"]').exists()).toBe(false)
  })

  it('saves only changed roles with PUT /permissions/plugins/:plugin', async () => {
    const w = await render()
    const dirty = () => w.emitted('dirty')?.at(-1)
    await box(w, EXPORT, 'operator').setValue(true)
    expect(dirty()).toEqual([true])
    expect(w.get('[data-testid="matrix-save"]').text()).toContain('1 unsaved changes')
    expect(w.get('[data-testid="matrix-save"]').text()).toContain('Operator · Export records')

    const saved = structuredClone(MATRIX)
    saved.roles[2].permission_keys = [OLD, READ, EXPORT]
    mocks.put.mockResolvedValueOnce(saved)
    await w.get('[data-testid="settings-save"]').trigger('click')
    await flushPromises()
    // The role's every key of the plugin, the removed one it keeps included; other roles left out.
    expect(mocks.put).toHaveBeenCalledWith('/permissions/plugins/guard', { roles: { operator: [OLD, READ, EXPORT] } })
    expect(mocks.toast).toHaveBeenCalledWith('Role grants saved', 'success')
    expect(dirty()).toEqual([false])
    expect(mocks.refresh).not.toHaveBeenCalled()
  })

  it('reloads the caller’s permissions after changing a role it holds', async () => {
    auth.roles = ['operator']
    const w = await render()
    await box(w, EXPORT, 'operator').setValue(true)
    mocks.put.mockResolvedValueOnce(structuredClone(MATRIX))
    await w.get('[data-testid="settings-save"]').trigger('click')
    await flushPromises()
    expect(mocks.refresh).toHaveBeenCalled()
  })

  it('a 403 shows the role by name and what was missing', async () => {
    const w = await render()
    await box(w, EXPORT, 'operator').setValue(true)
    mocks.put.mockRejectedValueOnce(new ApiError(403, { code: 'permission_denied', message: 'no', details: { missing: [EXPORT], role: 'operator' } }))
    await w.get('[data-testid="settings-save"]').trigger('click')
    await flushPromises()
    const text = w.get('[data-testid="matrix-save-missing"]').text()
    expect(text.startsWith('Operator')).toBe(true)
    expect(text).toContain(EXPORT)
    expect(mocks.toast).not.toHaveBeenCalled()
    // The edit stays to fix or reset.
    expect(box(w, EXPORT, 'operator').element.checked).toBe(true)
  })

  it('role columns not editable are read-only', async () => {
    const w = await render()
    expect(box(w, READ, 'lead').element.disabled).toBe(true)
    expect(box(w, READ, 'super_admin').element.disabled).toBe(true)
    expect(box(w, READ, 'super_admin').element.checked).toBe(true)
    expect(w.get('th[data-role="lead"]').attributes('title')).toBe('You cannot change this role')
  })

  it('removed rows can only be unticked', async () => {
    const w = await render()
    expect(box(w, OLD, 'admin').element.disabled).toBe(true)
    expect(box(w, OLD, 'operator').element.disabled).toBe(false)
    await box(w, OLD, 'operator').setValue(false)
    expect(box(w, OLD, 'operator').element.checked).toBe(false)
    expect(w.find('[data-testid="matrix-save"]').exists()).toBe(true)
  })

  it('a permission the editor does not hold can only be taken away', async () => {
    auth.denied.add(EXPORT)
    const w = await render()
    expect(box(w, EXPORT, 'operator').element.disabled).toBe(true)
    expect(box(w, EXPORT, 'admin').element.disabled).toBe(false)
  })

  it('without role:define the matrix is read-only', async () => {
    auth.denied.add('role:define')
    const w = await render()
    expect(box(w, READ, 'admin').element.disabled).toBe(true)
    expect(w.find('[data-testid="matrix-restore"]').exists()).toBe(false)
  })

  it('without role:read only host grants show', async () => {
    auth.denied.add('role:read')
    const w = await render()
    expect(mocks.get).not.toHaveBeenCalledWith('/permissions/plugins/guard')
    expect(w.find('[data-testid="plugin-user-permissions"]').exists()).toBe(false)
    expect(w.find('[data-testid="plugin-host-permissions"]').exists()).toBe(true)
  })

  it('a plugin declaring no user permissions says so', async () => {
    mocks.get.mockImplementation(async (path: string) => {
      if (path.startsWith('/permissions/plugins/')) throw new ApiError(404, { code: 'not_found', message: 'plugin not found' })
      return []
    })
    const w = mount(PermissionsTab, { props: { detail: { key: 'guard', status: 'enabled' } as unknown as PluginDetail }, global: { plugins: [i18n] } })
    await flushPromises()
    expect(w.get('[data-testid="plugin-user-permissions"]').text()).toContain('This plugin declares no permissions for users.')
    expect(mocks.toast).not.toHaveBeenCalled()
  })

  it('a matrix that fails to load says so with a retry, not "declares none"', async () => {
    mocks.get.mockImplementation(async (path: string) => {
      if (path.startsWith('/permissions/plugins/')) throw new ApiError(500, { code: 'internal', message: 'boom' })
      return []
    })
    const w = mount(PermissionsTab, { props: { detail: { key: 'guard', status: 'enabled' } as unknown as PluginDetail }, global: { plugins: [i18n] } })
    await flushPromises()
    const card = w.get('[data-testid="plugin-user-permissions"]')
    expect(card.text()).not.toContain('This plugin declares no permissions for users.')
    expect(card.find('[data-testid="matrix-load-failed"]').exists()).toBe(true)

    mocks.get.mockImplementation(async (path: string) => (path.startsWith('/permissions/plugins/') ? structuredClone(MATRIX) : []))
    await card.get('[data-testid="matrix-load-failed"] button').trigger('click')
    await flushPromises()
    expect(w.find('[data-testid="matrix-load-failed"]').exists()).toBe(false)
    expect(box(w, READ, 'operator').element.checked).toBe(true)
  })

  it('reloads the matrix with the detail unless it holds unsaved edits', async () => {
    const w = await render()
    const reads = () => mocks.get.mock.calls.filter(([path]) => path === '/permissions/plugins/guard').length
    expect(reads()).toBe(1)
    await w.setProps({ detail: { key: 'guard', status: 'enabled', grants: [] } as unknown as PluginDetail })
    await flushPromises()
    expect(reads()).toBe(2)

    await box(w, EXPORT, 'operator').setValue(true)
    await w.setProps({ detail: { key: 'guard', status: 'enabled', grants: [] } as unknown as PluginDetail })
    await flushPromises()
    expect(reads()).toBe(2)
    expect(box(w, EXPORT, 'operator').element.checked).toBe(true)
  })

  it('disabled plugin shows grants kept', async () => {
    const w = await render({ status: 'disabled' })
    expect(w.get('[data-testid="plugin-user-permissions"]').text()).toContain('grants are kept')
    expect(w.find('[data-testid="perm-matrix"]').exists()).toBe(true)
  })
})

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { ApiError } from '@sub2api/host'
import { i18n } from '@/i18n'
import GrantsTab from './GrantsTab.vue'
import type { PluginDetail, PluginGrant } from '../pluginUtil'

const mocks = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn(), del: vi.fn(), toast: vi.fn(), confirm: vi.fn(async () => true), lacks: new Set<string>() }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: { get: mocks.get, put: mocks.put, del: mocks.del } }))
vi.mock('@sub2api/ui', async (original) => ({ ...await original<object>(), toast: mocks.toast, confirm: mocks.confirm }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ has: (k: string) => !mocks.lacks.has(k) }) }))
enableAutoUnmount(afterEach)

const GRANTS: PluginGrant[] = [
  { permission: 'gateway.hook', scope: { points: ['gateway.request'], fields: ['model', 'prompt_text', 'system'] }, status: 'granted' },
  { permission: 'net', scope: { domains: ['hooks.example.com'] }, status: 'granted' },
  { permission: 'limits.x', scope: { limits: { rpm: 10 } }, status: 'granted' },
  { permission: 'broadcast', scope: null, status: 'revoked' }
]

async function render() {
  mocks.get.mockResolvedValue(GRANTS)
  const w = mount(GrantsTab, { props: { detail: { key: 'guard', grants: GRANTS } as unknown as PluginDetail }, attachTo: document.body, global: { plugins: [i18n], stubs: { teleport: true } } })
  await flushPromises()
  return w
}
type W = Awaited<ReturnType<typeof render>>
const row = (w: W, permission: string) => w.findAll('tbody tr').find((r) => r.text().includes(permission))!
const chip = (w: W, list: string, text: string) => w.get(`[data-list="${list}"]`).findAll('button[aria-pressed]').find((b) => b.text() === text)!

describe('plugin grants tab', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.lacks = new Set()
    i18n.global.locale.value = 'en'
    mocks.put.mockResolvedValue({})
  })

  it('narrow sends PUT with the remaining items', async () => {
    const w = await render()
    await row(w, 'gateway.hook').get('[data-testid="grant-narrow"]').trigger('click')
    await chip(w, 'fields', 'system').trigger('click')
    await w.get('[data-testid="narrow-submit"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/plugins/guard/grants/gateway.hook', { scope: { points: ['gateway.request'], fields: ['model', 'prompt_text'] } })
    expect(mocks.toast).toHaveBeenCalledWith('Scope narrowed', 'success')
    // The list is read again and the dialog closes.
    expect(mocks.get).toHaveBeenCalledTimes(2)
    expect(w.find('[data-list="fields"]').exists()).toBe(false)
  })

  it('narrow keeps at least one item per list', async () => {
    const w = await render()
    await row(w, 'gateway.hook').get('[data-testid="grant-narrow"]').trigger('click')
    // A list of one item cannot lose it.
    expect(chip(w, 'points', 'gateway.request').attributes('disabled')).toBeDefined()
    await chip(w, 'fields', 'model').trigger('click')
    await chip(w, 'fields', 'prompt_text').trigger('click')
    expect(chip(w, 'fields', 'system').attributes('disabled')).toBeDefined()
    // A removed item can be put back.
    await chip(w, 'fields', 'model').trigger('click')
    expect(chip(w, 'fields', 'system').attributes('disabled')).toBeUndefined()
  })

  it('no narrow button for non-list scopes or lists of one', async () => {
    const w = await render()
    expect(row(w, 'net').find('[data-testid="grant-narrow"]').exists()).toBe(false)
    expect(row(w, 'limits.x').find('[data-testid="grant-narrow"]').exists()).toBe(false)
    expect(row(w, 'broadcast').find('[data-testid="grant-narrow"]').exists()).toBe(false)
    expect(row(w, 'net').find('[data-testid="grant-regrant"]').exists()).toBe(false)
  })

  it('regrant on a revoked row sends PUT {}', async () => {
    const w = await render()
    await row(w, 'broadcast').get('[data-testid="grant-regrant"]').trigger('click')
    await flushPromises()
    expect(mocks.confirm).toHaveBeenCalled()
    expect(mocks.put).toHaveBeenCalledWith('/plugins/guard/grants/broadcast', {})
    expect(mocks.toast).toHaveBeenCalledWith('Permission granted again', 'success')
  })

  it('granting again takes plugin:install: without it the button is greyed out and says so', async () => {
    mocks.lacks = new Set(['plugin:install'])
    const w = await render()
    const button = row(w, 'broadcast').get('[data-testid="grant-regrant"]')
    expect(button.attributes('disabled')).toBeDefined()
    expect(button.attributes('title')).toBe(i18n.global.t('plugins.grants.needsPermission', { permission: 'plugin:install' }))
    await button.trigger('click')
    await flushPromises()
    expect(mocks.put).not.toHaveBeenCalled()
  })

  it('a 400 scope_too_wide shows the error and keeps the dialog', async () => {
    const w = await render()
    mocks.put.mockRejectedValueOnce(new ApiError(400, { code: 'invalid_argument', message: 'the scope is wider than requested', details: { fields: [{ field: 'scope', code: 'scope_too_wide', message: 'wider' }] } }))
    await row(w, 'gateway.hook').get('[data-testid="grant-narrow"]').trigger('click')
    await chip(w, 'fields', 'system').trigger('click')
    await w.get('[data-testid="narrow-submit"]').trigger('click')
    await flushPromises()
    expect(mocks.toast).toHaveBeenCalledWith('The scope is wider than the plugin requests', 'error')
    expect(w.find('[data-list="fields"]').exists()).toBe(true)
  })

  it('a 403 names the grant permission missing', async () => {
    const w = await render()
    mocks.put.mockRejectedValueOnce(new ApiError(403, { code: 'permission_denied', message: 'granting [broadcast] requires plugin:grant:high', details: { permission: 'plugin:grant:high', host_permissions: ['broadcast'] } }))
    await row(w, 'broadcast').get('[data-testid="grant-regrant"]').trigger('click')
    await flushPromises()
    expect(mocks.toast).toHaveBeenCalledWith('Needs the plugin:grant:high permission', 'error')
  })
})

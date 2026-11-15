import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { i18n } from '@/i18n'

const perms = vi.hoisted(() => ({ held: new Set<string>() }))
// me.permissions: the /me list; has (with the superuser shortcut) must not be used.
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ me: { permissions: [...perms.held] }, has: () => true })
}))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
enableAutoUnmount(afterEach)

import PluginIframe from './PluginIframe.vue'

/** Sends a bridge request from the frame and returns the console's response. */
async function ask(method: string, params: unknown) {
  const w = mount(PluginIframe, { props: { pluginKey: 'guard', src: 'about:blank' }, attachTo: document.body, global: { plugins: [i18n, createPinia()] } })
  const win = (w.get('iframe').element as HTMLIFrameElement).contentWindow!
  const sent = vi.spyOn(win, 'postMessage').mockImplementation(() => undefined)
  window.dispatchEvent(new MessageEvent('message', { data: { s2a: 1, kind: 'request', id: 'r1', method, params }, origin: 'null', source: win }))
  await flushPromises()
  return sent.mock.calls.map((c) => c[0] as { kind: string; id: string; result?: any; error?: any }).find((m) => m.kind === 'response' && m.id === 'r1')!
}

describe('PluginIframe permissions.check (T83)', () => {
  it('answers for this plugin’s local keys only', async () => {
    perms.held = new Set(['plugin.guard:stats:read', 'user:read', 'plugin.other:stats:read'])
    const r = await ask('permissions.check', { keys: ['stats:read', 'rules:write', 'user:read', 'plugin.guard:stats:read', 'plugin.other:stats:read', ''] })
    expect(r.result).toEqual({
      'stats:read': true,
      'rules:write': false,
      // A core key is read as a local key of this plugin, never as the core one.
      'user:read': false,
      'plugin.guard:stats:read': false,
      'plugin.other:stats:read': false,
      '': false
    })
  })

  it('an unscoped local key is held at any scope; a scoped one when a held key implies it', async () => {
    perms.held = new Set(['plugin.guard:flow:manage:own', 'plugin.guard:rule:read:value/input', 'plugin.other:stats:read:own'])
    const r = await ask('permissions.check', { keys: ['flow:manage', 'flow:manage:own', 'flow:manage:flow/3', 'rule:read', 'rule:read:value/other', 'flow', 'stats:read'] })
    expect(r.result).toEqual({
      'flow:manage': true,
      'flow:manage:own': true,
      'flow:manage:flow/3': false,
      'rule:read': true,
      'rule:read:value/other': false,
      // "flow" is no action of the held keys, only a prefix of one.
      flow: false,
      'stats:read': false
    })
  })

  it('the whole row or a covering scope holds a scoped key, as the server decides', async () => {
    perms.held = new Set(['plugin.guard:flow:manage', 'plugin.guard:rule:read:value/input'])
    const r = await ask('permissions.check', { keys: ['flow:manage:own', 'flow:manage:flow/3', 'rule:read:value/input', 'rule:read:own'] })
    expect(r.result).toEqual({ 'flow:manage:own': true, 'flow:manage:flow/3': true, 'rule:read:value/input': true, 'rule:read:own': false })
  })

  it('refuses more than 50 keys or no array', async () => {
    expect((await ask('permissions.check', { keys: Array.from({ length: 51 }, (_, i) => `k${i}:read`) })).error.code).toBe('invalid_argument')
    expect((await ask('permissions.check', { keys: 'stats:read' })).error.code).toBe('invalid_argument')
    expect((await ask('permissions.check', { keys: ['stats:read', ['stats:read']] })).error.code).toBe('invalid_argument')
    expect((await ask('permissions.check', { keys: Array.from({ length: 50 }, (_, i) => `k${i}:read`) })).result).toBeDefined()
  })
})

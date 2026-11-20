import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { defineComponent, h, reactive } from 'vue'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import { SSpinner } from '@sub2api/ui'
import { i18n } from '@/i18n'
import PluginPageView from './PluginPageView.vue'

// The plugin store as the page and PluginComponent read it.
const store = vi.hoisted(() => ({
  state: null as unknown as { listed: boolean; pending: Record<string, boolean>; errors: Record<string, string>; mismatch: Record<string, 'plugin' | 'core'>; pages: Record<string, unknown>; components: Record<string, unknown> }
}))
vi.mock('@/stores/plugins', () => ({
  assetURL: (p: { asset_base: string }, path: string) => `${p.asset_base}/${path}`,
  usePluginStore: () => ({
    get listed() { return store.state.listed },
    get pending() { return store.state.pending },
    get errors() { return store.state.errors },
    uiMismatch: (key: string) => store.state.mismatch[key] ?? null,
    page: (key: string, id: string) => (store.state.pages[id] ? { plugin: { key, version: '1.0.0', name: { en: 'Demo' }, asset_base: '/a', menus: [] }, page: store.state.pages[id] } : null),
    component: (_key: string, name?: string) => (name ? store.state.components[name] : undefined),
    refresh: vi.fn()
  })
}))
vi.mock('@/components/plugin/PluginIframe.vue', () => ({ default: { name: 'PluginIframe', props: ['src'], template: '<iframe :src="src" />' } }))
enableAutoUnmount(afterEach)

async function open(page: string) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/p/:plugin/:page', component: PluginPageView }] })
  await router.push(`/p/demo/${page}`)
  const w = mount(RouterView, { global: { plugins: [router, i18n] } })
  await flushPromises()
  return w
}

describe('plugin page', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'en'
    store.state = reactive({ listed: true, pending: {}, errors: {}, mismatch: {}, pages: {}, components: {} })
  })

  it('waits for the plugin list, then mounts a native page without the console header', async () => {
    store.state.listed = false
    store.state.pages.dash = { type: 'native', component: 'Dash' }
    store.state.components.Dash = defineComponent({ setup: () => () => h('p', { 'data-testid': 'dash' }, 'dashboard') })
    const w = await open('dash')
    expect(w.findComponent(SSpinner).exists()).toBe(true)
    store.state.listed = true
    await flushPromises()
    expect(w.get('[data-testid="dash"]').text()).toBe('dashboard')
    expect(w.find('h1').exists()).toBe(false)
  })

  it('shows why a native page has no component the way native forms do', async () => {
    store.state.pages.dash = { type: 'native', component: 'Dash' }
    store.state.mismatch.demo = 'plugin'
    const w = await open('dash')
    expect(w.get('[data-testid="plugin-component-failed"]').text()).toBe("This plugin version's interface is no longer supported. Upgrade the plugin.")
  })

  it('loads an iframe page from its src and names an older package\'s declarative page', async () => {
    store.state.pages.frame = { type: 'iframe', src: 'ui/iframe/index.html' }
    store.state.pages.list = { type: 'table' }
    expect((await open('frame')).get('iframe').attributes('src')).toBe('/a/ui/iframe/index.html')
    expect((await open('list')).text()).toContain('Unsupported page type: table')
  })
})

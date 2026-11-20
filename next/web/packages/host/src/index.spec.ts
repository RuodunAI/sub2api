import { describe, expect, it } from 'vitest'
import { belowRange, createPluginHost, definePluginHost, HOST_COMPONENT_NAMES, pluginPermissionKey, type HostContext, type PluginHost } from './index'

describe('plugin host', () => {
  it('hands every plugin the core components of the host (UI 3.7 editors, UI 3.11 / 3.12 entity pickers)', () => {
    const components = Object.freeze(Object.fromEntries(HOST_COMPONENT_NAMES.map(n => [n, { name: n }]))) as HostContext['components']
    const base = { components, i18n: { t: (k: string) => k }, permissions: { has: () => true } } as unknown as HostContext
    const host = createPluginHost(base, { key: 'p', version: '1', assetBase: '/a', trust: 'official' }, () => undefined)
    expect(host.components).toBe(components)
  })
})

describe('definePluginHost', () => {
  it('holds the host register() set, and refuses before it and after unregister', () => {
    const { setHost, useHost } = definePluginHost('shop')
    expect(() => useHost()).toThrow('shop UI used before register(host)')
    const host = { plugin: { key: 'shop' } } as unknown as PluginHost
    setHost(host)
    expect(useHost()).toBe(host)
    setHost(null)
    expect(() => useHost()).toThrow('shop UI used before register(host)')
  })

  it('keeps each plugin its own host', () => {
    const a = definePluginHost('a')
    const b = definePluginHost('b')
    a.setHost({} as PluginHost)
    expect(() => b.useHost()).toThrow('b UI used')
  })
})

describe('pluginPermissionKey', () => {
  it.each([
    ['orders:read', 'plugin.shop:orders:read'],
    ['plugin.shop:orders:read', 'plugin.shop:orders:read'],
    ['plugin.blog:posts:read', 'plugin.shop:plugin.blog:posts:read'],
    ['', ''],
    ['plugin.shop:', '']
  ])('qualifies %j', (local, want) => {
    expect(pluginPermissionKey('shop', local)).toBe(want)
  })

  it('backs host.can and denies an empty key', () => {
    const held = new Set(['plugin.shop:orders:read'])
    const base = { permissions: { has: (k: string) => k === '' || held.has(k) } } as unknown as HostContext
    const host = createPluginHost(base, { key: 'shop', version: '1', assetBase: '/a', trust: 'official' }, () => undefined)
    expect(host.can('orders:read')).toBe(true)
    expect(host.can('orders:write')).toBe(false)
    expect(host.can('')).toBe(false)
  })
})

describe('belowRange', () => {
  it.each([
    ['^4.1', true],
    ['4.1', true],
    ['>4.0.0', true],
    ['>=4.0.1 <5.0.0', true],
    ['^4.1 || ^5.0', true],
    ['^4.0', false],
    ['4.x', false],
    ['^3.20', false],
    ['<4.0.0', false],
    ['^3.20 || ^4.1', false],
    ['*', false],
    ['', false]
  ])('4.0.0 against %j: %s', (range, want) => {
    expect(belowRange('4.0.0', range)).toBe(want)
  })
})

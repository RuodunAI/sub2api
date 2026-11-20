import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import PlatformBadge from './PlatformBadge.vue'
import { platformBrand } from './brands'

// The real platform list on a mocked API: every badge asks for it.
const get = vi.hoisted(() => vi.fn())
vi.mock('@sub2api/host', async original => ({ ...(await original<typeof import('@sub2api/host')>()), api: { get } }))

const render = (props: InstanceType<typeof PlatformBadge>['$props']) => mount(PlatformBadge, { props, global: { plugins: [i18n] } })

describe('platform badge', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'zh'
    get.mockResolvedValue([{ id: 'gemini', label: { en: 'Google Gemini', zh: '谷歌 Gemini' }, builtin: true, endpoints: [] }])
  })

  it('loads the platform list once however many badges are shown', async () => {
    const badges = ['anthropic', 'openai', 'gemini'].map(id => render({ id }))
    await flushPromises()
    render({ id: 'typesafe' })
    expect(get.mock.calls).toEqual([['/me/platforms']])
    expect(badges[2].text()).toBe('谷歌 Gemini')
  })

  it("shows the mark and the label in the vendor's colours, the mark left to the label for assistive technology", async () => {
    const w = render({ id: 'gemini' })
    await flushPromises()
    expect(w.classes()).toContain('brand-tint')
    expect(w.attributes('style')).toContain(platformBrand('gemini').color)
    expect(w.attributes('data-platform')).toBe('gemini')
    expect(w.get('svg[data-brand="gemini"]').attributes('aria-hidden')).toBe('true')
    expect(w.text()).toBe('谷歌 Gemini')
  })

  it('names a platform the list does not have by the label of the payload, else by its id', () => {
    // The stand-in mark is the first letter of the same label.
    expect(render({ id: 'foo', label: { en: 'Foo', zh: '富' } }).text()).toBe('富富')
    expect(render({ id: 'foo' }).text()).toBe('Ffoo')
  })

  it('greys and strikes an unavailable platform, the reason behind a warning icon', () => {
    const w = render({ id: 'foo', label: { en: 'Foo', zh: 'Foo' }, unavailable: '插件未启用' })
    expect(w.classes()).not.toContain('brand-tint')
    expect(w.attributes('style')).toBeUndefined()
    expect(w.find('[data-brand]').exists()).toBe(false)
    expect(w.get('.line-through').text()).toBe('Foo')
    expect(w.get('[role="img"]').attributes('aria-label')).toBe('插件未启用')
  })
})

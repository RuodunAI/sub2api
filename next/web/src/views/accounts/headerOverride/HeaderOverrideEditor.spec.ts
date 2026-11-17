import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import HeaderOverrideField from './HeaderOverrideField.vue'

const toast = vi.hoisted(() => vi.fn())
vi.mock('@sub2api/ui', async orig => ({ ...(await orig<object>()), toast }))

enableAutoUnmount(afterEach)
const override = { 'User-Agent': 'my-client/1.0', 'x-trace-id': '{client_header:x-request-id}' }

function render(modelValue: Record<string, string> | null = override, props: Record<string, unknown> = {}) {
  const w = mount(HeaderOverrideField, {
    props: {
      modelValue,
      ...props,
      'onUpdate:modelValue': (v: unknown) => void w.setProps({ modelValue: v as Record<string, string> | null })
    },
    global: { plugins: [i18n] },
    attachTo: document.body
  })
  return w
}

const qa = (id: string) => [...document.body.querySelectorAll<HTMLElement>(`[data-testid="${id}"]`)]
const q = (id: string) => qa(id)[0] ?? null
const field = (el: HTMLElement) => (el.matches('input,textarea') ? el : el.querySelector('input,textarea')) as HTMLInputElement
const type = async (el: HTMLElement, value: string) => {
  const input = field(el)
  input.value = value
  input.dispatchEvent(new Event('input'))
  await flushPromises()
}
const click = async (el: Element | null | undefined) => {
  ;(el as HTMLElement).click()
  await flushPromises()
}
const view = (label: string) => [...q('ho-view')!.querySelectorAll('button,label')].find(b => b.textContent?.trim() === label)
const menuItem = (label: string) => [...document.body.querySelectorAll('[role^="menuitem"]')].find(b => b.textContent?.trim() === label)
const applied = (w: ReturnType<typeof render>) => w.emitted('update:modelValue')?.at(-1)?.[0]
async function openEditor(w: ReturnType<typeof render>) {
  await w.get('[data-testid="account-header-override-edit"]').trigger('click')
  await flushPromises()
}

describe('request header override editor', () => {
  beforeEach(() => {
    i18n.global.locale.value = 'zh'
  })

  it('summarizes the override on the row', () => {
    const w = render()
    expect(w.get('[data-testid="account-header-override-summary"]').text()).toBe('2 项')
    expect(w.text()).toContain('1. User-Agent 设为 my-client/1.0')
    expect(w.text()).toContain('2. x-trace-id 取客户端 x-request-id 的值')
    expect(render(null).get('[data-testid="account-header-override-summary"]').text()).toBe('未设置')
  })

  it('shows the server error and the reason of a fixed control', () => {
    const w = render(null, { error: 'Authorization: blocked', reason: '由账号类型固定', disabled: true })
    expect(w.text()).toContain('Authorization: blocked')
    expect(w.text()).toContain('由账号类型固定')
    expect(w.get('[data-testid="account-header-override-edit"]').attributes('disabled')).toBeDefined()
  })

  it('adds a header in the table and applies the override', async () => {
    const w = render()
    await openEditor(w)
    expect(qa('ho-row')).toHaveLength(2)
    await click(q('ho-add'))
    await type(qa('ho-key')[2], 'x-unwanted')
    await type(qa('ho-value')[2], 'x')
    expect(qa('ho-row-hint')[2].textContent).toContain('x-unwanted 设为 x')
    await click(q('ho-apply'))
    expect(applied(w)).toEqual({ ...override, 'x-unwanted': 'x' })
  })

  it('inserts placeholders beside the value', async () => {
    const w = render(null)
    await openEditor(w)
    expect(qa('ho-row')).toHaveLength(1)
    await type(q('ho-key')!, 'anthropic-beta')
    await click(q('ho-insert'))
    await click(menuItem('客户端请求头 + 追加'))
    expect(field(q('ho-value')!).value).toBe('{client_header:anthropic-beta:}')
    await click(q('ho-add'))
    await click(qa('ho-insert')[1])
    await click(menuItem('透传全部（键 *）'))
    expect(field(qa('ho-key')[1]).value).toBe('*')
    expect(qa('ho-row-hint')[1].textContent).toContain('透传全部客户端请求头')
    await click(q('ho-insert'))
    await click(menuItem('删除此头'))
    await click(q('ho-apply'))
    expect(applied(w)).toEqual({ 'anthropic-beta': '{unset}', '*': '' })
  })

  it('keeps Apply blocked on a duplicate name', async () => {
    const w = render()
    await openEditor(w)
    await type(qa('ho-key')[1], 'User-Agent')
    expect(qa('ho-row-hint')[1].textContent).toContain('请求头名重复')
    await click(q('ho-apply'))
    expect(w.emitted('update:modelValue')).toBeUndefined()
  })

  it('keeps a row problem out of JSON and Apply, such as a case-insensitive duplicate or an empty placeholder', async () => {
    const w = render()
    await openEditor(w)
    await type(qa('ho-key')[1], 'user-agent')
    expect(qa('ho-row-hint')[1].textContent).toContain('请求头名重复')
    await click(view('JSON'))
    expect(q('ho-json')).toBeNull()
    expect(toast).toHaveBeenLastCalledWith('第 2 行有误：请求头名重复（不区分大小写）', 'error')
    await type(qa('ho-key')[1], '')
    await click(qa('ho-insert')[1])
    await click(menuItem('客户端请求头'))
    expect(field(qa('ho-value')[1]).value).toBe('{client_header:}')
    await type(qa('ho-key')[1], 'x-a')
    expect(qa('ho-row-hint')[1].textContent).toContain('缺少客户端请求头名')
    await type(qa('ho-value')[1], '{client_header:x-b:}')
    expect(qa('ho-row-hint')[1].textContent).toContain('追加值为空')
    await click(q('ho-apply'))
    expect(toast).toHaveBeenLastCalledWith('第 2 行有误：第二个冒号后的追加值为空：填上追加值，或改用 {client_header:名}', 'error')
    expect(w.emitted('update:modelValue')).toBeUndefined()
  })

  it('names the row in the labels of its buttons', async () => {
    const w = render()
    await openEditor(w)
    await click(q('ho-add'))
    expect(qa('ho-remove').map(el => el.getAttribute('aria-label'))).toEqual(['删除 User-Agent', '删除 x-trace-id', '删除 3'])
    expect(qa('ho-insert')[0].getAttribute('aria-label')).toBe('为 User-Agent 插入占位符')
  })

  it('caps the rows at 100', async () => {
    const w = render(Object.fromEntries(Array.from({ length: 100 }, (_, i) => [`x-h${i}`, 'v'])))
    await openEditor(w)
    expect(qa('ho-row')).toHaveLength(100)
    expect(q('ho-add')!.hasAttribute('disabled')).toBe(true)
  })

  it('mirrors valid JSON into the table, keeping the rows across views', async () => {
    const w = render(null)
    await openEditor(w)
    await type(q('ho-key')!, 'x-a')
    await type(q('ho-value')!, '1')
    await click(view('JSON'))
    expect(JSON.parse(field(q('ho-json')!).value)).toEqual({ 'x-a': '1' })
    await type(q('ho-json')!, '{"x-a": 1}')
    expect(document.body.textContent).toContain('x-a 的值需要是字符串')
    await click(view('表格'))
    expect(q('ho-rows')).toBeNull()
    await type(q('ho-json')!, '{"*": "", "x-b": "{unset}"}')
    await click(view('表格'))
    expect(qa('ho-key').map(el => field(el).value)).toEqual(['*', 'x-b'])
    await click(q('ho-apply'))
    expect(applied(w)).toEqual({ '*': '', 'x-b': '{unset}' })
  })

  it('applies null once every row is removed', async () => {
    const w = render()
    await openEditor(w)
    await click(q('ho-remove'))
    await click(q('ho-remove'))
    await click(q('ho-apply'))
    expect(applied(w)).toBeNull()
  })
})

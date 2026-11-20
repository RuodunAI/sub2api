import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import type { Account } from '@/api/types'
import { i18n } from '@/i18n'
import AccountModelTest from './AccountModelTest.vue'

const mocks = vi.hoisted(() => ({ post: vi.fn(), toast: vi.fn(), notifyError: vi.fn() }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: { post: mocks.post } }))
vi.mock('@sub2api/ui', async (original) => ({ ...await original<typeof import('@sub2api/ui')>(), toast: mocks.toast }))
vi.mock('@/utils/errors', () => ({ notifyError: mocks.notifyError, errorMessage: String }))
vi.mock('@/composables/useScope', async (original) => ({ ...await original<typeof import('@/composables/useScope')>(), useScope: () => ({ can: () => true }) }))
enableAutoUnmount(afterEach)
afterEach(() => vi.restoreAllMocks())

/** A test request that never answers: it rejects like fetch once its signal aborts. */
function hangingTest() {
  const signals: AbortSignal[] = []
  mocks.post.mockImplementation((_path: string, _body: unknown, opts?: { signal?: AbortSignal }) => new Promise((_resolve, reject) => {
    const signal = opts!.signal!
    signals.push(signal)
    signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
  }))
  return signals
}

const account = { id: 7, name: 'slow', models: ['gpt-image-1', 'gpt-image-2'], model_mapping: {} } as unknown as Account

function render() {
  return shallowMount(AccountModelTest, { props: { open: false, account }, global: { plugins: [i18n], stubs: {
    SModal: { template: '<div><slot /><slot name="footer" /></div>' },
    STable: { props: ['rows'], template: '<div><div v-for="row in rows" :key="row.model" :data-row="row.model"><slot name="cell-state" :row="row" /><slot name="cell-actions" :row="row" /></div></div>' },
    SButton: { template: '<button><slot /></button>' },
    SBadge: { template: '<span><slot /></span>' },
    SHint: { template: '<span><slot /></span>' },
  } } })
}

describe('account model test without a time limit', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    i18n.global.locale.value = 'zh'
  })

  it('sends a test with no timeout and stops it from its row: canceled, not failed', async () => {
    const signals = hangingTest()
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    const w = render()
    await w.setProps({ open: true })
    await w.get('[data-row="gpt-image-1"] [data-testid="model-test-one"]').trigger('click')
    expect(signals).toHaveLength(1)
    expect(timeout).not.toHaveBeenCalled()
    expect(signals[0].aborted).toBe(false)
    expect(w.find('[data-testid="model-test-stop"]').exists()).toBe(true)

    await w.get('[data-row="gpt-image-1"] [data-testid="model-test-cancel"]').trigger('click')
    await flushPromises()
    expect(signals[0].aborted).toBe(true)
    const row = w.get('[data-row="gpt-image-1"]')
    expect(row.text()).toContain('已取消')
    expect(row.text()).not.toContain('失败')
    expect(row.find('[data-testid="model-test-one"]').exists()).toBe(true)
    expect(w.emitted('tested')).toBeUndefined()
    expect(w.find('[data-testid="model-test-stop"]').exists()).toBe(false)
  })

  it('stops a whole run: tests in flight are aborted and no more start', async () => {
    const signals = hangingTest()
    const w = render()
    await w.setProps({ open: true })
    await w.get('[data-testid="model-test-all"]').trigger('click')
    expect(signals).toHaveLength(2)
    await w.get('[data-testid="model-test-stop"]').trigger('click')
    await flushPromises()
    expect(signals.every((s) => s.aborted)).toBe(true)
    expect(mocks.post).toHaveBeenCalledTimes(2)
    expect(w.text()).toContain('已取消 2')
    expect(mocks.toast).toHaveBeenCalledWith('测试已停止：成功 0，失败 0', 'info')
  })

  it('aborts the tests when the dialog closes or the page is left', async () => {
    const signals = hangingTest()
    const w = render()
    await w.setProps({ open: true })
    await w.get('[data-row="gpt-image-1"] [data-testid="model-test-one"]').trigger('click')
    await w.setProps({ open: false })
    expect(signals[0].aborted).toBe(true)

    await w.setProps({ open: true })
    await w.get('[data-row="gpt-image-2"] [data-testid="model-test-one"]').trigger('click')
    w.unmount()
    expect(signals[1].aborted).toBe(true)
  })
})

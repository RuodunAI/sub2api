import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { i18n } from '@/i18n'
import { useBatchTest } from './useBatchTest'

const mocks = vi.hoisted(() => ({ post: vi.fn(), toast: vi.fn() }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: { post: mocks.post } }))
vi.mock('@sub2api/ui', async (original) => ({ ...await original<typeof import('@sub2api/ui')>(), toast: mocks.toast }))
enableAutoUnmount(afterEach)
afterEach(() => vi.restoreAllMocks())

/** Test requests that never answer: each rejects like fetch once its signal aborts. */
function hangingTests() {
  const signals: AbortSignal[] = []
  mocks.post.mockImplementation((_path: string, _body: unknown, opts?: { signal?: AbortSignal }) => new Promise((_resolve, reject) => {
    const signal = opts!.signal!
    signals.push(signal)
    signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
  }))
  return signals
}

function setup() {
  const tested = vi.fn()
  let batch!: ReturnType<typeof useBatchTest>
  const w = mount(defineComponent({ setup() { batch = useBatchTest({ tested }); return () => null } }), { global: { plugins: [i18n] } })
  return { w, batch, tested }
}

describe('batch account test without a time limit', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    i18n.global.locale.value = 'zh'
  })

  it('stops the batch: tests in flight are aborted, none recorded as failed, no more start', async () => {
    const signals = hangingTests()
    const timeout = vi.spyOn(AbortSignal, 'timeout')
    const { batch, tested } = setup()
    const run = batch.batchTest([1, 2, 3, 4, 5])
    await flushPromises()
    expect(signals).toHaveLength(3)
    expect(timeout).not.toHaveBeenCalled()
    expect([...batch.batchTesting.value]).toEqual([1, 2, 3])

    batch.stopBatch()
    await run
    expect(signals.every((s) => s.aborted)).toBe(true)
    expect(mocks.post).toHaveBeenCalledTimes(3)
    expect(tested).not.toHaveBeenCalled()
    expect(batch.batchRunning.value).toBe(false)
    expect(batch.batchTesting.value.size).toBe(0)
    expect(mocks.toast).toHaveBeenCalledWith('批量测试已停止：成功 0，失败 0', 'info')
  })

  it('aborts the batch when the page is left', async () => {
    const signals = hangingTests()
    const { w, batch } = setup()
    void batch.batchTest([1])
    await flushPromises()
    w.unmount()
    expect(signals[0].aborted).toBe(true)
  })
})

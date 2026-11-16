import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import type { Account } from '@/api/types'
import AccountDeleteDialog from './AccountDeleteDialog.vue'

const mocks = vi.hoisted(() => ({ del: vi.fn() }))
vi.mock('@sub2api/host', async (importOriginal) => ({ ...await importOriginal<typeof import('@sub2api/host')>(), api: mocks }))

enableAutoUnmount(afterEach)
const account = { id: 7, name: 'cc-7' } as Account

function render() {
  return mount(AccountDeleteDialog, { props: { open: true, account }, global: { plugins: [i18n], stubs: { Teleport: true } } })
}

describe('account deletion', () => {
  beforeEach(() => {
    mocks.del.mockReset().mockResolvedValue({})
    i18n.global.locale.value = 'zh'
  })
  it('deletes the account', async () => {
    const wrapper = render()
    expect(wrapper.get('[role="dialog"]').attributes('aria-describedby')).toBeTruthy()
    await wrapper.get('[data-testid="account-delete-confirm"]').trigger('click')
    await flushPromises()
    expect(mocks.del).toHaveBeenCalledWith('/accounts/7', undefined)
    expect(wrapper.emitted('deleted')).toHaveLength(1)
  })
  it('cannot be closed while the request runs', async () => {
    let finish: (v: unknown) => void = () => {}
    mocks.del.mockReturnValue(new Promise(resolve => (finish = resolve)))
    const wrapper = render()
    await wrapper.get('[data-testid="account-delete-confirm"]').trigger('click')
    expect(wrapper.find('[data-overlay-close]').exists()).toBe(false)
    await wrapper.get('.modal-overlay').trigger('mousedown')
    expect(wrapper.emitted('update:open')).toBeUndefined()
    finish({})
    await flushPromises()
    expect(wrapper.emitted('update:open')).toEqual([[false]])
    expect(wrapper.emitted('deleted')).toHaveLength(1)
  })
  it('a failed deletion keeps the dialog open', async () => {
    mocks.del.mockRejectedValue(new Error('unavailable'))
    const wrapper = render()
    await wrapper.get('[data-testid="account-delete-confirm"]').trigger('click')
    await flushPromises()
    expect(wrapper.emitted('deleted')).toBeUndefined()
    expect(wrapper.emitted('update:open')).toBeUndefined()
  })
})

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { ApiError } from '@sub2api/host'
import { i18n } from '@/i18n'
import type { Account } from '@/api/types'
import AccountOwnerDialog from './AccountOwnerDialog.vue'

const mocks = vi.hoisted(() => ({ put: vi.fn() }))
vi.mock('@sub2api/host', async (importOriginal) => ({ ...await importOriginal<typeof import('@sub2api/host')>(), api: mocks }))

enableAutoUnmount(afterEach)
const EntityPicker = { props: ['id'], emits: ['update:id'], template: '<input :value="id" @change="$emit(\'update:id\', $event.target.value)" />' }

async function render(account: Partial<Account>) {
  const w = mount(AccountOwnerDialog, { props: { open: false, account: { id: 7, name: 'cc-7', ...account } as Account }, global: { plugins: [i18n], stubs: { Teleport: true, EntityPicker } } })
  await w.setProps({ open: true })
  const submit = async () => {
    await w.get('form').trigger('submit')
    await flushPromises()
  }
  return { w, submit }
}

describe('changing the owner of an account (CONTRACTS §102.4)', () => {
  beforeEach(() => {
    mocks.put.mockReset().mockResolvedValue({ id: 7, owner_id: 9, owner_email: 'nine@x' })
    i18n.global.locale.value = 'zh'
  })

  it('starts from the current owner and hands on the new one the server answers', async () => {
    const { w, submit } = await render({ owner_id: 3 })
    const picker = w.get('[data-testid="account-owner-picker"]')
    expect((picker.element as HTMLInputElement).value).toBe('3')
    await picker.setValue('9')
    await submit()
    expect(mocks.put).toHaveBeenCalledWith('/accounts/7/owner', { owner_id: 9 })
    expect(w.emitted('changed')).toEqual([[{ id: 7, owner_id: 9, owner_email: 'nine@x' }]])
    expect(w.emitted('update:open')).toEqual([[false]])
  })

  it('needs an owner, and says so when the user is gone', async () => {
    const { w, submit } = await render({ owner_id: null })
    await submit()
    expect(mocks.put).not.toHaveBeenCalled()
    expect(w.text()).toContain(i18n.global.t('common.required'))
    mocks.put.mockRejectedValue(new ApiError(400, { code: 'invalid_argument', message: 'invalid', details: { fields: [{ field: 'owner_id', code: 'not_found', message: 'no such user' }] } }))
    await w.get('[data-testid="account-owner-picker"]').setValue('99')
    await submit()
    expect(w.text()).toContain(i18n.global.t('accounts.ownerChange.notFound'))
    expect(w.emitted('changed')).toBeUndefined()
  })
})

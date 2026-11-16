import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import type { User } from '@/api/types'

const mocks = vi.hoisted(() => ({
  users: [] as object[],
  assignable: [] as object[],
  put: vi.fn(async () => ({})),
  patch: vi.fn(async (_path: string, _body: Record<string, unknown>) => ({})),
  epoch: 0,
  superuser: false,
  perms: null as string[] | null
}))
vi.mock('@sub2api/host', async (original) => {
  const real = await original<typeof import('@sub2api/host')>()
  const api = {
    ...real.api,
    get: vi.fn(async (path: string) => (path === '/me/assignable-roles' ? mocks.assignable : [])),
    list: vi.fn(async () => ({ items: mocks.users, page: { page: 1, page_size: 20, total: mocks.users.length }, paged: true })),
    put: mocks.put,
    patch: mocks.patch
  }
  return { ...real, api }
})
vi.mock('@/composables/lookups', async (original) => ({
  ...(await original<object>()),
  fetchFirstPage: async () => ['admin', 'operator', 'supplier', 'user', 'super_admin'].map((key) => ({ key, name: key, superuser: key === 'super_admin' }))
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    has: (p: string) => mocks.superuser || !mocks.perms || mocks.perms.includes(p),
    hasAction: (a: string) => mocks.superuser || !mocks.perms || mocks.perms.some((p) => p === a || p.startsWith(`${a}:`)),
    me: { id: 1, superuser: mocks.superuser },
    permissionsEpoch: mocks.epoch
  })
}))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: {} }), useRouter: () => ({ push: vi.fn() }) }))
enableAutoUnmount(afterEach)

import UsersView from './UsersView.vue'

const SDropdown = { props: ['actions'], emits: ['select'], template: '<div class="row-actions"><button v-for="a in actions.filter(x => !x.hidden)" :key="a.key" :data-action="a.key" @click="$emit(\'select\', a.key)">{{ a.key }}</button></div>' }
const EntityPicker = { props: ['id'], emits: ['update:id'], template: '<input :value="id" @change="$emit(\'update:id\', $event.target.value)" />' }
const SFormModal = { props: ['open'], emits: ['submit'], template: '<form v-if="open" data-testid="modal" @submit.prevent="$emit(\'submit\')"><slot /></form>' }
const managed = (key: string, grantable = true) => ({ key, name: key, builtin: true, grantable, missing: grantable ? [] : ['user:delete'] })
const user = (id: number, roles: string[]) => ({ id, email: `u${id}@x`, display_name: '', roles, status: 'active' }) as unknown as User

async function render() {
  const w = mount(UsersView, {
    global: {
      plugins: [i18n],
      stubs: { SDropdown, SFormModal, EntityPicker, Teleport: true, UserCreateDialog: true, UserPricingDrawer: true, UserSegmentMoveDialog: true, AdjustBalanceModal: true, ResetPasswordDialog: true, TransferSuperAdminDialog: true },
      directives: { permission: {} }
    }
  })
  await flushPromises()
  return w
}
const row = (w: Awaited<ReturnType<typeof render>>, id: number) => w.findAll('.row-actions')[mocks.users.findIndex((u) => (u as User).id === id)]
const actions = (w: Awaited<ReturnType<typeof render>>, id: number) => row(w, id).findAll('button').map((b) => b.attributes('data-action'))

beforeEach(() => {
  i18n.global.locale.value = 'zh'
  mocks.epoch++
  mocks.superuser = false
  mocks.perms = null
  mocks.put.mockClear()
  mocks.patch.mockClear()
  mocks.assignable = [managed('operator'), managed('supplier'), managed('user')]
})

describe('UsersView managed users (T78)', () => {
  it('offers user-changing actions only on users the caller manages', async () => {
    mocks.users = [user(2, ['operator', 'user']), user(3, ['admin']), user(1, ['admin'])]
    const w = await render()
    expect(actions(w, 2)).toEqual(['edit', 'roles', 'pricing', 'ledger', 'password', 'security', 'balance', 'delete'])
    // An admin peer and the caller itself (admin is not on admin's list): read-only actions.
    expect(actions(w, 3)).toEqual(['pricing', 'ledger'])
    expect(actions(w, 1)).toEqual(['pricing', 'ledger'])
  })

  it('without balance:adjust:any, offers the balance adjustment on plain users only', async () => {
    mocks.perms = ['user:update', 'balance:adjust']
    mocks.users = [user(2, ['operator', 'user']), user(4, ['supplier', 'user']), user(5, ['user']), user(6, [])]
    const w = await render()
    expect(actions(w, 2)).not.toContain('balance')
    expect(actions(w, 4)).not.toContain('balance')
    expect(actions(w, 5)).toContain('balance')
    expect(actions(w, 6)).toContain('balance')
  })

  it('a role the caller may not grant makes its holders unmanaged', async () => {
    mocks.assignable = [managed('operator', false), managed('user')]
    mocks.users = [user(2, ['operator'])]
    const w = await render()
    expect(actions(w, 2)).not.toContain('balance')
  })

  it('the super admin manages everyone else; its own row only hands the role over (UsersView.superAdmin.spec)', async () => {
    mocks.superuser = true
    mocks.users = [user(1, ['super_admin']), user(3, ['admin'])]
    const w = await render()
    expect(actions(w, 1)).toEqual(['transfer'])
    expect(actions(w, 3)).toEqual(['edit', 'roles', 'pricing', 'ledger', 'password', 'security', 'balance', 'delete'])
  })

  it('locks roles the caller does not manage and submits them unchanged', async () => {
    mocks.superuser = true
    mocks.users = [user(2, ['admin', 'user'])]
    const w = await render()
    await row(w, 2).get('[data-action="roles"]').trigger('click')
    await flushPromises()
    const box = (k: string) => w.get(`[data-testid="modal"] input[value="${k}"]`)
    expect(box('admin').attributes('disabled')).toBeDefined()
    expect((box('admin').element as HTMLInputElement).checked).toBe(true)
    await box('operator').setValue(true)
    await w.get('[data-testid="modal"]').trigger('submit')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/users/2/roles', { role_keys: ['admin', 'user', 'operator'] })
  })

  it('checking supplier checks the default role with it', async () => {
    mocks.users = [user(2, [])]
    const w = await render()
    await row(w, 2).get('[data-action="roles"]').trigger('click')
    await flushPromises()
    await w.get('[data-testid="modal"] input[value="supplier"]').setValue(true)
    await w.get('[data-testid="modal"]').trigger('submit')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/users/2/roles', { role_keys: ['supplier', 'user'] })
  })

  it('an ungrantable managed role cannot be added', async () => {
    mocks.assignable = [managed('operator', false), managed('user')]
    mocks.users = [user(2, ['user'])]
    mocks.superuser = true // keep the row manageable to reach the dialog
    const w = await render()
    await row(w, 2).get('[data-action="roles"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-testid="modal"] input[value="operator"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-testid="modal"]').text()).toContain('user:delete')
  })

  it('holders of the default role are not managed when it is listed as not grantable', async () => {
    mocks.assignable = [managed('supplier'), managed('user', false)]
    mocks.users = [user(2, ['user'])]
    const w = await render()
    expect(actions(w, 2)).toEqual(['pricing', 'ledger'])
  })
})

describe('UsersView owners (CONTRACTS §102.3)', () => {
  const edit = async (w: Awaited<ReturnType<typeof render>>, id: number) => {
    await row(w, id).get('[data-action="edit"]').trigger('click')
    await flushPromises()
  }
  const save = async (w: Awaited<ReturnType<typeof render>>) => {
    await w.get('[data-testid="modal"]').trigger('submit')
    await flushPromises()
  }

  it('a team holder edits and assigns the roles of its team, never deletes or resets a password, and names no owner', async () => {
    mocks.perms = ['user:read:team', 'user:create:team', 'user:update:team', 'role:read', 'role:assign']
    mocks.assignable = [managed('supplier'), managed('supplier_agent')]
    mocks.users = [{ ...user(6, ['supplier']), owner_id: 11, owner_email: 'agent@x' }]
    const w = await render()
    expect(actions(w, 6)).toEqual(['edit', 'roles'])
    expect(w.text()).toContain('agent@x')
    expect(w.find('[data-testid="user-direct-filter"]').exists()).toBe(true)
    await edit(w, 6)
    expect(w.find('[data-testid="user-edit-owner"]').exists()).toBe(false)
    await save(w)
    expect(mocks.patch.mock.calls[0][1].owner_id).toBeUndefined()
  })

  it('a holder over every user moves a user under another owner, or takes the owner away', async () => {
    mocks.users = [{ ...user(6, ['supplier']), owner_id: 11 }]
    const w = await render()
    expect(w.find('[data-testid="user-direct-filter"]').exists()).toBe(false)
    await edit(w, 6)
    await w.get('[data-testid="user-edit-owner"]').setValue('12')
    await save(w)
    expect(mocks.patch).toHaveBeenLastCalledWith('/users/6', expect.objectContaining({ owner_id: 12 }))
    await edit(w, 6)
    await w.get('[data-testid="user-edit-owner"]').setValue('')
    await save(w)
    expect(mocks.patch).toHaveBeenLastCalledWith('/users/6', expect.objectContaining({ owner_id: 0 }))
  })
})

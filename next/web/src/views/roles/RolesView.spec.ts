import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { i18n } from '@/i18n'
import { ApiError } from '@sub2api/host'
import { toastState } from '@sub2api/ui'
import RolesView from './RolesView.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn(async (_path: string): Promise<unknown> => []), post: vi.fn(async (_path: string) => ({ id: 1 })), put: vi.fn(async () => ({})), patch: vi.fn(async () => ({})), list: vi.fn(async () => ({ items: [], page: { total: 0 } })) }))
vi.mock('@sub2api/host', async (original) => ({ ...await original<typeof import('@sub2api/host')>(), api: mocks }))
const ui = vi.hoisted(() => ({ confirm: vi.fn(async (_o: object) => true) }))
vi.mock('@sub2api/ui', async (original) => ({ ...(await original<object>()), confirm: (o: object) => ui.confirm(o) }))
const lookups = vi.hoisted(() => ({ roles: [] as object[] }))
vi.mock('@/composables/lookups', async (original) => ({ ...await original<object>(), fetchFirstPage: async () => lookups.roles }))
const auth = vi.hoisted(() => ({ me: null as object | null, perms: null as string[] | null }))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get me() { return auth.me },
    get permissionsEpoch() { return 0 },
    has: (p: string | string[]) => !auth.perms || [p].flat().some(k => auth.perms!.includes(k)),
    hasDynamic: (k: string) => !auth.perms || auth.perms.includes(k),
    refreshPermissions: async () => undefined
  })
}))
enableAutoUnmount(afterEach)

const SFormModal = { props: ['open'], emits: ['submit'], template: '<form v-if="open" @submit.prevent="$emit(\'submit\')"><slot /></form>' }

async function openCreate() {
  auth.me = null
  auth.perms = null
  lookups.roles = []
  const w = mount(RolesView, { global: { plugins: [i18n, createPinia()], stubs: { SFormModal, teleport: true } } })
  await flushPromises()
  await w.get('[data-testid="role-create"]').trigger('click')
  return w
}

async function create(key: string, name = 'Role') {
  const w = await openCreate()
  await w.get('[data-testid="role-create-key"]').setValue(key)
  await w.get('[data-testid="role-create-name"]').setValue(name)
  await w.find('form').trigger('submit')
  await flushPromises()
  return w
}

describe('RolesView role key', () => {
  it('accepts the server limit of 50 characters', async () => {
    mocks.post.mockClear()
    const key = 'a' + 'b'.repeat(49)
    await create(key)
    expect(mocks.post).toHaveBeenCalledWith('/roles', expect.objectContaining({ key }))
  })

  it('rejects longer, one-letter or non-letter-leading keys before posting', async () => {
    for (const key of ['a' + 'b'.repeat(50), 'a', '1ab', 'Ab']) {
      mocks.post.mockClear()
      const w = await create(key)
      expect(mocks.post).not.toHaveBeenCalled()
      expect(w.text()).toContain(i18n.global.t('roles.keyInvalid'))
    }
  })
})

describe('RolesView create dialog', () => {
  it('notes that the key is fixed and has one name field', async () => {
    const w = await openCreate()
    expect(w.text()).toContain(i18n.global.t('roles.keyHint'))
    expect(i18n.global.t('roles.keyHint', {}, { locale: 'zh' })).toContain('创建后不可修改')
    expect(w.findAll('form input').length).toBe(2)
  })

  it('saves the one name in every locale', async () => {
    mocks.post.mockClear()
    await create('auditor')
    expect(mocks.post).toHaveBeenCalledWith('/roles', { key: 'auditor', name: { zh: 'Role', en: 'Role' }, description: { zh: '', en: '' } })
  })

  it('requires the name', async () => {
    mocks.post.mockClear()
    const w = await create('auditor', '  ')
    expect(mocks.post).not.toHaveBeenCalled()
    expect(w.text()).toContain(i18n.global.t('common.required'))
  })
})

describe('RolesView reserved key', () => {
  it('shows the reserved key error in the console language', async () => {
    const prev = i18n.global.locale.value
    mocks.post.mockRejectedValueOnce(new ApiError(400, { code: 'invalid_argument', message: 'key is reserved', details: { fields: [{ field: 'key', code: 'reserved', message: 'key is reserved' }] } }))
    i18n.global.locale.value = 'zh'
    const w = await create('supplier')
    expect(w.text()).toContain(i18n.global.t('roles.keyReserved'))
    expect(w.text()).not.toContain('key is reserved')
    i18n.global.locale.value = prev
  })
})

describe('RolesView permissions (T77)', () => {
  const role = (over: object) => ({ id: 1, key: 'lead', name: 'Lead', builtin: false, superuser: false, permission_keys: ['user:read'], assignable_role_keys: ['user'], assign_custom: false, ...over })
  const roles = [
    role({ id: 1, key: 'admin', name: 'Admin', builtin: true }),
    role({ id: 2, key: 'lead', name: 'Lead' }),
    role({ id: 3, key: 'super_admin', name: 'Super', builtin: true, superuser: true })
  ]

  let meID = 100
  async function open(id: number, { superuser = false, perms = ['role:define'], manages = [] as string[], list = roles as object[], stubs = {}, mine = [] as string[], modules = [] as object[] } = {}) {
    // A new identity each time: the managed-roles list is cached per user.
    auth.me = { id: ++meID, roles: mine, superuser, permissions: perms }
    mocks.get.mockImplementation(async (path: string) =>
      path === '/me/assignable-roles'
        ? manages.map(key => ({ key, name: key, builtin: true, grantable: true, missing: [], missing_assignable: [], missing_assign_custom: false }))
        : path === '/permissions' ? modules : [])
    auth.perms = perms
    lookups.roles = list
    const w = mount(RolesView, { global: { plugins: [i18n, createPinia()], stubs: { SFormModal, teleport: true, ...stubs } } })
    await flushPromises()
    const target = (list as typeof roles).find(r => r.id === id)!
    await w.findAll('button').find(b => b.text().includes(target.key))!.trigger('click')
    await flushPromises()
    return w
  }

  it('shows a built-in role read-only to a non-super admin', async () => {
    const w = await open(1)
    expect(w.find('[data-testid="role-save"]').exists()).toBe(false)
    expect(w.find('[data-testid="role-menu"]').exists()).toBe(false)
    expect(w.find('[data-testid="role-restore"]').exists()).toBe(false)
    expect(w.find('[data-testid="role-builtin-locked"]').exists()).toBe(true)
    expect(w.get('[data-testid="role-assignable"] input[data-role="admin"]').attributes('disabled')).toBeDefined()
  })

  it('lets the super admin edit a built-in role', async () => {
    const w = await open(1, { superuser: true })
    expect(w.find('[data-testid="role-save"]').exists()).toBe(true)
    expect(w.find('[data-testid="role-restore"]').exists()).toBe(true)
    expect(w.find('[data-testid="role-builtin-locked"]').exists()).toBe(false)
  })

  it('lets a definer edit and restore a built-in role it manages, not one it does not', async () => {
    expect((await open(1, { manages: ['admin'] })).find('[data-testid="role-restore"]').exists()).toBe(true)
    const locked = await open(1, { manages: ['operator'] })
    expect(locked.find('[data-testid="role-save"]').exists()).toBe(false)
    expect(locked.find('[data-testid="role-builtin-locked"]').exists()).toBe(true)
  })

  it('restores defaults only after a confirmation', async () => {
    const w = await open(1, { superuser: true })
    mocks.post.mockClear()
    ui.confirm.mockResolvedValueOnce(false)
    await w.get('[data-testid="role-restore"]').trigger('click')
    await flushPromises()
    expect(ui.confirm).toHaveBeenLastCalledWith(expect.objectContaining({ danger: true, message: expect.stringContaining('Admin') }))
    expect(mocks.post).not.toHaveBeenCalled()
    await w.get('[data-testid="role-restore"]').trigger('click')
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledWith('/roles/1/reset')
  })

  it('saves the managed roles with PUT /roles/:id/assignable including assign_custom', async () => {
    mocks.put.mockClear()
    const w = await open(2)
    // The super admin role is never offered.
    expect(w.find('[data-testid="role-assignable"] [data-role="super_admin"]').exists()).toBe(false)
    await w.get('[data-testid="role-assignable"] input[data-role="lead"]').setValue(true)
    await w.get('[data-testid="role-assign-custom"]').trigger('click')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledTimes(1)
    expect(mocks.put).toHaveBeenCalledWith('/roles/2/assignable', { role_keys: ['lead', 'user'], assign_custom: true })
  })

  it('shows what a refused save lacked', async () => {
    mocks.put.mockRejectedValueOnce(new ApiError(403, { code: 'permission_denied', message: 'no', details: { missing_assignable: ['admin'] } }))
    const w = await open(2)
    await w.get('[data-testid="role-assignable"] input[data-role="admin"]').setValue(true)
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-testid="role-save-missing"]').text()).toContain('admin')
  })

  it('names the role a refused save was refused for', async () => {
    mocks.put.mockRejectedValueOnce(new ApiError(403, { code: 'permission_denied', message: 'no', details: { missing: ['plugin.guard:bypass'], role: 'lead' } }))
    const w = await open(2)
    await w.get('[data-testid="role-assignable"] input[data-role="admin"]').setValue(true)
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    const missing = i18n.global.t('roles.missingPermissions', { keys: 'plugin.guard:bypass' })
    expect(w.get('[data-testid="role-save-missing"]').text()).toBe(i18n.global.t('roles.missingFor', { role: 'lead', text: missing }))
  })

  describe('plugin permissions', () => {
    const PermissionTree = { name: 'PermissionTree', props: ['granted', 'modules', 'allGranted', 'readOnly', 'scopeLimit', 'roleKey', 'grantable'], emits: ['update:granted'], template: '<i />' }

    it('passes the role key and what the editor may grant to the tree', async () => {
      const w = await open(2, { perms: ['role:define', 'plugin.guard:rules:read'], stubs: { PermissionTree } })
      const tree = w.getComponent(PermissionTree)
      expect(tree.props('roleKey')).toBe('lead')
      const grantable = tree.props('grantable') as (k: string) => boolean
      expect([grantable('plugin.guard:rules:read'), grantable('plugin.guard:bypass')]).toEqual([true, false])
    })

    it('lets the editor grant a disabled plugin\'s key its own role holds, though /me leaves it out', async () => {
      const list = [...roles, role({ id: 5, key: 'mgr', permission_keys: ['plugin.foo:dashboard:read', 'plugin.foo:gone'] })]
      const modules = [{ module: 'plugin.foo', label: 'Foo', source: 'plugin', status: 'disabled', permissions: [
        { key: 'plugin.foo:dashboard:read', label: 'View', risk: 'low', status: 'disabled' },
        { key: 'plugin.foo:gone', label: 'Gone', risk: 'low', status: 'removed' }
      ] }]
      const w = await open(2, { perms: ['role:define'], mine: ['mgr'], list, modules, stubs: { PermissionTree } })
      const grantable = w.getComponent(PermissionTree).props('grantable') as (k: string) => boolean
      // A removed permission's keys do not count, as on the server.
      expect([grantable('plugin.foo:dashboard:read'), grantable('plugin.foo:gone'), grantable('plugin.foo:other')]).toEqual([true, false, false])
    })

    it('saves plugin scoped keys with the rest through PUT /roles/:id/permissions', async () => {
      mocks.put.mockClear()
      const w = await open(2, { stubs: { PermissionTree } })
      w.getComponent(PermissionTree).vm.$emit('update:granted', new Set(['user:read', 'plugin.guard:rules:read', 'plugin.guard:rules:manage:own']))
      await flushPromises()
      await w.get('[data-testid="role-save"]').trigger('click')
      await flushPromises()
      expect(mocks.put).toHaveBeenCalledWith('/roles/2/permissions', { permission_keys: ['plugin.guard:rules:manage:own', 'plugin.guard:rules:read', 'user:read'] })
    })
  })

  it('without role:define: no create button and no editing', async () => {
    const w = await open(2, { perms: ['role:read'] })
    expect(w.find('[data-testid="role-create"]').exists()).toBe(false)
    expect(w.find('[data-testid="role-save"]').exists()).toBe(false)
  })

  it('sends only the changed parts, and after a partial failure shows what was saved', async () => {
    mocks.put.mockClear()
    mocks.patch.mockClear()
    mocks.put.mockRejectedValueOnce(new ApiError(403, { code: 'permission_denied', message: 'no', details: { missing: ['a:b'], missing_assign_custom: true } }))
    const w = await open(2)
    const name = w.findAll('input').find(i => (i.element as HTMLInputElement).value === 'Lead')!
    await name.setValue('Lead 2')
    await w.get('[data-testid="role-assign-custom"]').trigger('click')
    lookups.roles = roles.map(r => (r.id === 2 ? { ...r, name: 'Lead 2' } : r))
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledTimes(1)
    // Permissions unchanged: only the managed roles were PUT (and refused).
    expect(mocks.put.mock.calls.map(c => (c as unknown[])[0])).toEqual(['/roles/2/assignable'])
    expect(w.get('[data-testid="role-save-missing"]').text()).toContain('a:b')
    // The saved name is shown; the refused switch is back to what the role has.
    expect(w.text()).toContain('Lead 2')
    expect(w.get('[data-testid="role-assign-custom"]').attributes('aria-checked')).toBe('false')
  })

  it('offers delete on a custom role and restore on a built-in role, neither on the super admin role', async () => {
    const custom = await open(2)
    expect(custom.find('[data-testid="role-menu"]').exists()).toBe(true)
    expect(custom.find('[data-testid="role-restore"]').exists()).toBe(false)
    const builtin = await open(1, { superuser: true })
    expect(builtin.find('[data-testid="role-menu"]').exists()).toBe(false)
    expect(builtin.find('[data-testid="role-restore"]').exists()).toBe(true)
    const su = await open(3, { superuser: true })
    expect(su.find('[data-testid="role-assignable"]').exists()).toBe(false)
    expect(su.find('[data-testid="role-restore"]').exists()).toBe(false)
  })

  it('shows the key greyed out and fixed, and one name field', async () => {
    const w = await open(2)
    const key = w.get('[data-testid="role-key"]')
    expect((key.element as HTMLInputElement).value).toBe('lead')
    expect(key.attributes('disabled')).toBeDefined()
    expect(w.text()).toContain(i18n.global.t('roles.keyLocked'))
    expect((w.get('[data-testid="role-name"]').element as HTMLInputElement).value).toBe('Lead')
  })

  it('saves a changed name in every locale and nothing it did not change', async () => {
    mocks.patch.mockClear()
    const w = await open(2)
    await w.get('[data-testid="role-name"]').setValue(' Lead 2 ')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledWith('/roles/2', { name: { zh: 'Lead 2', en: 'Lead 2' } })
  })

  it('shows a built-in name in the console language and keeps it when only the description changes', async () => {
    const prev = i18n.global.locale.value
    i18n.global.locale.value = 'zh'
    mocks.patch.mockClear()
    const list = [...roles, role({ id: 4, key: 'auditor', name: { en: 'Auditor', zh: '审计员' }, builtin: true })]
    const w = await open(4, { superuser: true, list })
    expect((w.get('[data-testid="role-name"]').element as HTMLInputElement).value).toBe('审计员')
    await w.get('[data-testid="role-description"]').setValue('只读')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledWith('/roles/4', { description: { zh: '只读', en: '只读' } })
    i18n.global.locale.value = prev
  })

  it('does not count stray spaces as a change', async () => {
    mocks.patch.mockClear()
    const w = await open(2)
    await w.get('[data-testid="role-name"]').setValue('Lead ')
    expect(w.get('[data-testid="role-save"]').attributes('disabled')).toBeDefined()
    await w.get('[data-testid="role-description"]').setValue('Leads')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledWith('/roles/2', { description: { zh: 'Leads', en: 'Leads' } })
  })

  it('requires a name before saving', async () => {
    mocks.patch.mockClear()
    const w = await open(2)
    await w.get('[data-testid="role-name"]').setValue('  ')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).not.toHaveBeenCalled()
    expect(w.text()).toContain(i18n.global.t('common.required'))
  })

  it('shows a refused key change next to the key in the console language', async () => {
    const prev = i18n.global.locale.value
    i18n.global.locale.value = 'zh'
    const message = "a role's key cannot be changed after creation"
    mocks.patch.mockRejectedValueOnce(new ApiError(400, { code: 'invalid_argument', message, details: { fields: [{ field: 'key', code: 'immutable', message }] } }))
    const w = await open(2)
    await w.get('[data-testid="role-name"]').setValue('Lead 2')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('角色标识创建后不可修改')
    expect(w.text()).not.toContain(message)
    // The edit stays for another try.
    expect((w.get('[data-testid="role-name"]').element as HTMLInputElement).value).toBe('Lead 2')
    i18n.global.locale.value = prev
  })
})

describe('RolesView saving permissions', () => {
  const role = (over: object) => ({ id: 2, key: 'lead', name: 'Lead', builtin: false, superuser: false, permission_keys: ['user:read'], assignable_role_keys: [], assign_custom: false, revision: 7, ...over })
  const modules = [
    { module: 'user', label: 'Users', source: 'core', status: 'active', permissions: [{ key: 'user:read', label: 'View', risk: 'low', status: 'active' }, { key: 'user:delete', label: 'Delete', risk: 'sensitive', status: 'active' }] },
    { module: 'plugin.g', label: 'G', source: 'plugin', plugin_key: 'g', status: 'active', permissions: [{ key: 'plugin.g:bypass', label: 'Bypass', risk: 'critical', status: 'active' }, { key: 'plugin.g:read', label: 'Read', risk: 'low', status: 'active' }] }
  ]
  const PermissionTree = { name: 'PermissionTree', props: ['granted', 'modules', 'allGranted', 'readOnly', 'scopeLimit', 'roleKey', 'grantable'], emits: ['update:granted'], template: '<i />' }

  async function open(list: object[]) {
    auth.me = { id: 900 + Math.random(), roles: [], superuser: true, permissions: [] }
    auth.perms = ['role:define']
    lookups.roles = list
    mocks.get.mockImplementation(async (path: string) => (path === '/permissions' ? modules : []))
    const w = mount(RolesView, { global: { plugins: [i18n, createPinia()], stubs: { SFormModal, teleport: true, PermissionTree } } })
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('lead'))!.trigger('click')
    await flushPromises()
    return w
  }
  const grant = async (w: Awaited<ReturnType<typeof open>>, keys: string[]) => {
    w.getComponent(PermissionTree).vm.$emit('update:granted', new Set(keys))
    await flushPromises()
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
  }

  it('asks before granting sensitive or critical permissions, naming them, and sends the revision read', async () => {
    mocks.put.mockClear()
    ui.confirm.mockClear()
    ui.confirm.mockResolvedValueOnce(false)
    const w = await open([role({})])
    await grant(w, ['user:read', 'user:delete', 'plugin.g:bypass', 'plugin.g:read'])
    const asked = ui.confirm.mock.calls[0][0] as { message: string; danger: boolean }
    expect(asked.message).toMatch(/Bypass[\s\S]*Delete/)
    expect(asked.message).not.toContain('Read')
    expect(asked.danger).toBe(true)
    expect(mocks.put).not.toHaveBeenCalled()
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/roles/2/permissions', { permission_keys: ['plugin.g:bypass', 'plugin.g:read', 'user:delete', 'user:read'], revision: 7 })
  })

  it('carries the revision each part answered with into the next part it saves', async () => {
    mocks.put.mockClear()
    mocks.patch.mockClear()
    mocks.put.mockResolvedValueOnce(role({ revision: 8 }))
    const w = await open([role({})])
    await w.get('[data-testid="role-name"]').setValue('Lead 2')
    await grant(w, ['user:read', 'plugin.g:read'])
    expect(mocks.put).toHaveBeenCalledWith('/roles/2/permissions', expect.objectContaining({ revision: 7 }))
    expect(mocks.patch).toHaveBeenCalledWith('/roles/2', expect.objectContaining({ revision: 8 }))
  })

  it('a version conflict that left the role as it was saves again at once with the new revision (N2)', async () => {
    mocks.put.mockClear()
    toastState.items.splice(0)
    mocks.put.mockRejectedValueOnce(new ApiError(409, { code: 'version_conflict', message: 'changed', details: { revision: 9 } }))
    const w = await open([role({})])
    // Meanwhile the role changed otherwise: its permissions read the same, at a new revision.
    lookups.roles = [role({ revision: 9 })]
    await grant(w, ['user:delete'])
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledTimes(2)
    expect(mocks.put).toHaveBeenLastCalledWith('/roles/2/permissions', expect.objectContaining({ revision: 9 }))
    expect(toastState.items.map((x) => x.message)).not.toContain(i18n.global.t('roles.roleConflictMerged'))
  })

  it('a version conflict over a part someone else changed too is not saved again at once; a part the editor left alone takes what is read', async () => {
    mocks.patch.mockClear()
    mocks.put.mockClear()
    toastState.items.splice(0)
    mocks.patch.mockRejectedValueOnce(new ApiError(409, { code: 'version_conflict', message: 'changed', details: { revision: 9 } }))
    const w = await open([role({ assignable_role_keys: ['user'] })])
    // Meanwhile someone renamed the role and took the user role off its list.
    lookups.roles = [role({ name: 'Leader', assignable_role_keys: [], revision: 9 })]
    await w.get('[data-testid="role-name"]').setValue('Lead 2')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledTimes(1)
    expect((w.get('[data-testid="role-name"]').element as HTMLInputElement).value).toBe('Lead 2')
    expect(toastState.items.map((x) => x.message)).toContain(i18n.global.t('roles.roleConflictClashes', { list: i18n.global.t('common.name') }))
    // Saved again, it sends the name at the revision read; the managed roles, untouched here, stay as read.
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenLastCalledWith('/roles/2', expect.objectContaining({ revision: 9 }))
    expect(mocks.put).not.toHaveBeenCalled()
  })

  it('a version conflict over parts the editor left alone saves its own part again at once', async () => {
    mocks.patch.mockClear()
    toastState.items.splice(0)
    mocks.patch.mockRejectedValueOnce(new ApiError(409, { code: 'version_conflict', message: 'changed', details: { revision: 9 } }))
    const w = await open([role({})])
    lookups.roles = [role({ description: 'Theirs', assignable_role_keys: ['user'], revision: 9 })]
    await w.get('[data-testid="role-name"]').setValue('Lead 2')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.patch).toHaveBeenCalledTimes(2)
    expect(mocks.patch).toHaveBeenLastCalledWith('/roles/2', { name: expect.anything(), revision: 9 })
    expect(toastState.items.map((x) => x.message)).not.toContain(i18n.global.t('roles.roleConflictMerged'))
  })

  it('a version conflict reads the role again and keeps the edited cells on top', async () => {
    mocks.put.mockClear()
    mocks.put.mockRejectedValueOnce(new ApiError(409, { code: 'version_conflict', message: 'changed', details: { revision: 9 } }))
    const w = await open([role({})])
    // Meanwhile someone granted plugin.g:read; the editor adds user:delete and drops user:read.
    lookups.roles = [role({ permission_keys: ['plugin.g:read', 'user:read'], revision: 9 })]
    await grant(w, ['user:delete'])
    expect(w.getComponent(PermissionTree).props('granted')).toEqual(new Set(['plugin.g:read', 'user:delete']))
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenLastCalledWith('/roles/2/permissions', { permission_keys: ['plugin.g:read', 'user:delete'], revision: 9 })
  })
})

describe('RolesView copy from role', () => {
  const role = (over: object) => ({ id: 2, key: 'lead', name: 'Lead', builtin: false, superuser: false, permission_keys: ['user:read', 'plugin.g:read'], assignable_role_keys: [], assign_custom: false, revision: 7, ...over })
  const modules = [
    { module: 'user', label: 'Users', source: 'core', status: 'active', permissions: [{ key: 'user:read', label: 'View', risk: 'low', status: 'active' }, { key: 'user:delete', label: 'Delete', risk: 'sensitive', status: 'active' }] },
    { module: 'plugin.g', label: 'G', source: 'plugin', plugin_key: 'g', status: 'active', permissions: [{ key: 'plugin.g:read', label: 'Read', risk: 'low', status: 'active' }, { key: 'plugin.g:old', label: 'Old', risk: 'low', status: 'removed' }] }
  ]
  const list = [
    role({}),
    role({ id: 3, key: 'src', name: 'Source', permission_keys: ['user:read', 'user:delete', 'plugin.g:old'] }),
    role({ id: 4, key: 'super_admin', name: 'Super', builtin: true, superuser: true, permission_keys: undefined })
  ]
  const PermissionTree = { name: 'PermissionTree', props: ['granted', 'modules', 'allGranted', 'readOnly', 'scopeLimit', 'roleKey', 'grantable'], emits: ['update:granted'], template: '<i />' }
  const SSelect = { name: 'SSelect', props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<i />' }
  const SRadioGroup = { name: 'SRadioGroup', props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<i />' }

  async function open(me: { superuser: boolean; permissions: string[] }) {
    auth.me = { id: 1900 + Math.random(), roles: [], ...me }
    auth.perms = ['role:define']
    lookups.roles = list
    mocks.get.mockImplementation(async (path: string) => (path === '/permissions' ? modules : []))
    const w = mount(RolesView, { global: { plugins: [i18n, createPinia()], stubs: { SFormModal, teleport: true, PermissionTree, SSelect, SRadioGroup } } })
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('lead'))!.trigger('click')
    await flushPromises()
    return w
  }
  async function copy(w: Awaited<ReturnType<typeof open>>, from: number, mode?: 'union' | 'intersect') {
    await w.get('[data-testid="role-copy"]').trigger('click')
    const select = w.getComponent(SSelect)
    expect((select.props('options') as { value: number }[]).map((o) => o.value)).toEqual([3])
    select.vm.$emit('update:modelValue', from)
    if (mode) w.getComponent(SRadioGroup).vm.$emit('update:modelValue', mode)
    await flushPromises()
    await w.find('form').trigger('submit')
    await flushPromises()
  }
  const granted = (w: Awaited<ReturnType<typeof open>>) => [...(w.getComponent(PermissionTree).props('granted') as Set<string>)].sort()

  it('union adds the source role\'s permissions to the ticks, leaving out removed ones, and saves only on Save', async () => {
    mocks.put.mockClear()
    toastState.items.splice(0)
    const w = await open({ superuser: true, permissions: [] })
    await copy(w, 3)
    expect(granted(w)).toEqual(['plugin.g:read', 'user:delete', 'user:read'])
    expect(mocks.put).not.toHaveBeenCalled()
    expect(toastState.items.at(-1)?.message).toContain('plugin.g:old')
    await w.get('[data-testid="role-save"]').trigger('click')
    await flushPromises()
    expect(mocks.put).toHaveBeenCalledWith('/roles/2/permissions', { permission_keys: ['plugin.g:read', 'user:delete', 'user:read'], revision: 7 })
  })

  it('intersect keeps only what both roles grant', async () => {
    const w = await open({ superuser: true, permissions: [] })
    await copy(w, 3, 'intersect')
    expect(granted(w)).toEqual(['user:read'])
  })

  it('leaves out and names what the editor may not grant', async () => {
    toastState.items.splice(0)
    const w = await open({ superuser: false, permissions: ['role:define', 'user:read', 'plugin.g:read'] })
    await copy(w, 3)
    expect(granted(w)).toEqual(['plugin.g:read', 'user:read'])
    const last = toastState.items.at(-1)!
    expect(last.kind).toBe('warning')
    expect(last.message).toContain('user:delete')
  })

  it('is not offered on the super admin role', async () => {
    auth.me = { id: 2900 + Math.random(), roles: [], superuser: true, permissions: [] }
    auth.perms = ['role:define']
    lookups.roles = list
    mocks.get.mockImplementation(async (path: string) => (path === '/permissions' ? modules : []))
    const w = mount(RolesView, { global: { plugins: [i18n, createPinia()], stubs: { SFormModal, teleport: true, PermissionTree } } })
    await flushPromises()
    await w.findAll('button').find((b) => b.text().includes('super_admin'))!.trigger('click')
    await flushPromises()
    expect(w.find('[data-testid="role-copy"]').exists()).toBe(false)
  })
})

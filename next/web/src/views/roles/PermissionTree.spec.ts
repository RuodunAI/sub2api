import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import type { PermissionItem, PermissionModule } from '@/api/types'
import { parsePermission, permissionIndex } from '@/authz/implies'
import { RESTORE_ACTION, RESTORE_CASES } from '@/authz/testing/restoreCases'
import PermissionTree from './PermissionTree.vue'
import ScopePicker from './ScopePicker.vue'

enableAutoUnmount(afterEach)
vi.mock('@/components/entity/lookup', () => ({ useEntityNames: () => ({ label: (_kind: string, id: number) => `#${id}`, gone: () => false }) }))
const ui = vi.hoisted(() => ({ toast: vi.fn() }))
vi.mock('@sub2api/ui', async (original) => ({ ...(await original<object>()), toast: ui.toast }))

const perm = (key: string, status = 'active'): PermissionItem => ({ key, label: key, risk: 'low', status })
const mod: PermissionModule = {
  module: 'demo',
  label: 'Demo',
  source: 'plugin',
  status: 'active',
  permissions: [perm('p.active'), perm('p.removed', 'removed'), perm('p.disabled', 'disabled')]
}

const render = (granted: string[]) => mount(PermissionTree, { props: { modules: [mod], granted: new Set(granted) }, global: { plugins: [i18n] } })
// Inputs: the module's "select all" first, then one per permission in order.
const boxes = (w: ReturnType<typeof render>) => w.findAll<HTMLInputElement>('input[type="checkbox"]')
const lastEmitted = (w: ReturnType<typeof render>) => [...(w.emitted('update:granted')!.at(-1)![0] as Set<string>)].sort()

describe('PermissionTree removed permissions', () => {
  it('cannot tick a removed permission and marks it', () => {
    const w = render([])
    const removed = boxes(w)[2]
    expect(removed.element.disabled).toBe(true)
    expect(w.text()).toContain(i18n.global.t('roles.removed'))
    // Disabled plugin permissions stay selectable.
    expect(boxes(w)[3].element.disabled).toBe(false)
  })

  it('can untick a removed permission that is still granted', async () => {
    const w = render(['p.removed'])
    const removed = boxes(w)[2]
    expect(removed.element.disabled).toBe(false)
    await removed.setValue(false)
    expect(lastEmitted(w)).toEqual([])
  })

  it('select all skips removed permissions and then counts as all', async () => {
    const w = render([])
    await boxes(w)[0].setValue(true)
    expect(lastEmitted(w)).toEqual(['p.active', 'p.disabled'])

    const full = render(['p.active', 'p.disabled'])
    expect(boxes(full)[0].element.checked).toBe(true)
    await boxes(full)[0].setValue(false)
    expect(lastEmitted(full)).toEqual([])
  })

  it('clearing the module also drops a granted removed permission', async () => {
    const w = render(['p.active', 'p.removed', 'p.disabled'])
    expect(boxes(w)[0].element.checked).toBe(true)
    await boxes(w)[0].setValue(false)
    expect(lastEmitted(w)).toEqual([])
  })
})

describe('PermissionTree step-up marks', () => {
  it('marks step-up permissions and explains the mark only when there is one', () => {
    const selfMod: PermissionModule = { module: 'self', label: 'Self', source: 'core', status: 'active', permissions: [perm('self:profile'), { ...perm('self:security'), step_up: true }] }
    const w = mount(PermissionTree, { props: { modules: [selfMod, mod], granted: new Set<string>() }, global: { plugins: [i18n] } })
    expect(w.findAll('[data-testid="permission-step-up"]')).toHaveLength(1)
    expect(w.text()).toContain(i18n.global.t('roles.stepUpLegend'))
    expect(render([]).text()).not.toContain(i18n.global.t('roles.stepUpLegend'))
  })
})

describe('PermissionTree scoped actions', () => {
  const accountMod: PermissionModule = {
    module: 'account',
    label: 'Accounts',
    source: 'core',
    status: 'active',
    permissions: [{ ...perm('account:read'), scope_type: 'account' }, perm('account:relay')]
  }
  const limitOf = (keys: string[]) => (action: string) => keys.filter((k) => k.startsWith(action)).map((k) => parsePermission(k)!.scope)
  const renderScoped = (granted: string[], limit?: string[]) =>
    mount(PermissionTree, { props: { modules: [accountMod], granted: new Set(granted), scopeLimit: limit && limitOf(limit) }, global: { plugins: [i18n], stubs: { EntityPicker: true } } })

  it('shows one row per action, its scopes open while ticked', () => {
    const w = renderScoped(['account:read:own', 'account:read:group/3'])
    expect(boxes(w)[1].element.checked).toBe(true)
    expect(w.find('[data-testid="scope-account:read"]').exists()).toBe(true)
    expect(renderScoped([]).find('[data-testid="scope-account:read"]').exists()).toBe(false)
  })

  it('ticks an action at the widest scope the editor holds, and unticks every scope of it', async () => {
    const w = renderScoped([], ['account:read:own'])
    await boxes(w)[1].setValue(true)
    expect(lastEmitted(w)).toEqual(['account:read:own'])
    const full = renderScoped(['account:read:own', 'account:read:group/3'])
    await boxes(full)[1].setValue(false)
    expect(lastEmitted(full)).toEqual([])
    const all = renderScoped([])
    await boxes(all)[0].setValue(true)
    expect(lastEmitted(all)).toEqual(['account:read', 'account:relay'])
  })

  it('opens the scopes to choose from when the editor holds no own or full scope', async () => {
    const w = renderScoped([], ['account:read:group/3'])
    await boxes(w)[1].setValue(true)
    expect(lastEmitted(w)).toEqual([])
    expect(w.find('[data-testid="scope-account:read"]').text()).toContain(i18n.global.t('roles.scope.none'))
  })

  it('emits the scopes chosen in the picker as canonical keys beside the others', async () => {
    const w = renderScoped(['account:read', 'account:relay'])
    w.findComponent(ScopePicker).vm.$emit('update:scopes', [parsePermission('account:read:own')!.scope])
    expect(lastEmitted(w)).toEqual(['account:read:own', 'account:relay'])
  })

  it('keeps the scopes open while narrowing a loaded "all", and closes them when the row is unticked', async () => {
    const w = renderScoped(['account:read'])
    w.findComponent(ScopePicker).vm.$emit('update:scopes', [])
    expect(lastEmitted(w)).toEqual([])
    await w.setProps({ granted: new Set<string>() })
    expect(w.find('[data-testid="scope-account:read"]').text()).toContain(i18n.global.t('roles.scope.none'))
    await boxes(w)[1].setValue(true)
    await w.setProps({ granted: new Set(['account:read']) })
    await boxes(w)[1].setValue(false)
    await w.setProps({ granted: new Set<string>() })
    expect(w.find('[data-testid="scope-account:read"]').exists()).toBe(false)
  })
})

describe('PermissionTree descriptions', () => {
  it('shows the key and the description on hover', () => {
    const described: PermissionModule = { ...mod, permissions: [{ ...perm('p.active'), description: { en: 'Does things', zh: '做事' } }, perm('p.plain')] }
    const w = mount(PermissionTree, { props: { modules: [described], granted: new Set<string>() }, global: { plugins: [i18n] } })
    const titles = w.findAll('span[title]').map((s) => s.attributes('title'))
    expect(titles).toContain('p.active\nDoes things')
    expect(titles).toContain('p.plain')
  })
})

describe('PermissionTree plugin modules', () => {
  const G = 'plugin.guard'
  const guard: PermissionModule = {
    module: G,
    label: 'Guard',
    source: 'plugin',
    plugin_key: 'guard',
    status: 'active',
    groups: [{ key: 'rules', label: 'Rules', sort: 10 }, { key: 'ops', label: 'Operations', sort: 20 }],
    permissions: [
      { ...perm(`${G}:rules:read`), label: 'View rules', group: 'rules', defaults: { admin: [`${G}:rules:read`], operator: [`${G}:rules:read`] } },
      {
        ...perm(`${G}:rules:manage`),
        label: 'Manage rules',
        risk: 'sensitive',
        group: 'rules',
        requires: [`${G}:rules:read`],
        scope_type: 'plugin:ruleset',
        scope: { resource: 'ruleset', own: true },
        defaults: { admin: [`${G}:rules:manage`], operator: [`${G}:rules:manage:own`] }
      },
      { ...perm(`${G}:stats:read`), group: 'ops', defaults: { admin: [`${G}:stats:read`] } },
      { ...perm(`${G}:bypass`), risk: 'critical', group: 'ops' },
      perm(`${G}:loose`)
    ]
  }
  const core: PermissionModule = { module: 'user', label: 'Users', source: 'core', status: 'active', permissions: [perm('user:read'), { ...perm('user:delete'), risk: 'sensitive' }] }
  const limitOf = (keys: string[]) => (action: string) => keys.filter((k) => parsePermission(k)?.action === action).map((k) => parsePermission(k)!.scope)
  const renderGuard = (granted: string[], props: Record<string, unknown> = {}) =>
    mount(PermissionTree, { props: { modules: [core, guard], granted: new Set(granted), ...props }, global: { plugins: [i18n], stubs: { EntityPicker: true } } })
  const box = (w: ReturnType<typeof renderGuard>, key: string) => w.get<HTMLInputElement>(`input[data-permission="${key}"]`)
  const marks = (w: ReturnType<typeof renderGuard>) => Object.fromEntries(w.findAll(`[data-module="${G}"] label`).filter((l) => l.find('[data-testid="permission-mark"]').exists()).map((l) => [l.get('input').attributes('data-permission'), l.get('[data-testid="permission-mark"]').text()]))

  it('groups plugin permissions under their group headings, ungrouped last', () => {
    const w = renderGuard([])
    const sections = w.findAll(`[data-module="${G}"] [data-testid="permission-section"]`)
    expect(w.get('[data-group="rules"]').text()).toContain('Rules')
    expect(w.get('[data-group="rules"]').findAll('input[data-permission]').map((i) => i.attributes('data-permission'))).toEqual([`${G}:rules:read`, `${G}:rules:manage`])
    expect(w.get('[data-group="ops"]').findAll('input[data-permission]').map((i) => i.attributes('data-permission'))).toEqual([`${G}:stats:read`, `${G}:bypass`])
    expect(sections.at(-1)!.attributes('data-group')).toBeUndefined()
    expect(sections.at(-1)!.text()).toContain(`${G}:loose`)
    // A core module has no group headings.
    expect(w.find('[data-module="user"] [data-testid="group-select-all"]').exists()).toBe(false)
  })

  it('ticks a group as concrete keys, a scoped one at the widest scope the editor holds (D1)', async () => {
    const w = renderGuard([], { scopeLimit: limitOf([`${G}:rules:read`, `${G}:rules:manage:own`]) })
    await w.get('[data-group="rules"] input[data-testid="group-select-all"]').setValue(true)
    expect(lastEmitted(w)).toEqual([`${G}:rules:manage:own`, `${G}:rules:read`])
    const full = renderGuard([`${G}:rules:manage:own`, `${G}:rules:read`])
    expect(full.get<HTMLInputElement>('[data-group="rules"] input[data-testid="group-select-all"]').element.checked).toBe(true)
    await full.get('[data-group="rules"] input[data-testid="group-select-all"]').setValue(false)
    expect(lastEmitted(full)).toEqual([])
  })

  it('shows risk badges, low ones only in plugin modules', () => {
    const w = renderGuard([])
    expect(w.findAll('[data-module="user"] [data-risk]').map((b) => b.attributes('data-risk'))).toEqual(['sensitive'])
    expect(w.findAll(`[data-module="${G}"] [data-risk]`).map((b) => b.attributes('data-risk'))).toEqual(['low', 'sensitive', 'low', 'critical', 'low'])
    expect(w.text()).toContain(i18n.global.t('roles.riskLegend'))
  })

  it('marks "suggested" and "differs" against the defaults of the edited built-in role only', () => {
    const t = i18n.global.t
    const w = renderGuard([`${G}:rules:read`, `${G}:rules:manage`, `${G}:bypass`], { roleKey: 'operator' })
    // rules:manage is granted wider than suggested; stats:read has no suggestion for operator and is not granted; bypass suggests nothing.
    expect(marks(w)).toEqual({ [`${G}:rules:read`]: t('roles.suggested'), [`${G}:rules:manage`]: t('roles.differs') })
    expect(w.get('[data-testid="module-differs"]').text()).toContain(t('roles.differsCount', { n: 1 }, 1))
    expect(marks(renderGuard([`${G}:rules:read`], { roleKey: 'lead' }))).toEqual({})
    expect(renderGuard([], { roleKey: 'operator', allGranted: true }).find('[data-testid="permission-mark"]').exists()).toBe(false)
  })

  it('restores a module to the suggestion locally, leaving cells the editor cannot restore whole', async () => {
    ui.toast.mockClear()
    const grantable = (k: string) => k !== `${G}:stats:read`
    const w = renderGuard(['user:read', `${G}:rules:manage`, `${G}:loose`], { roleKey: 'admin', grantable })
    // stats:read is out of reach: neither marked nor counted.
    expect(w.get('[data-testid="module-differs"]').text()).toContain(i18n.global.t('roles.differsCount', { n: 1 }, 1))
    expect(marks(w)[`${G}:stats:read`]).toBeUndefined()
    await w.get('[data-testid="module-restore"]').trigger('click')
    // Keys without a suggestion (loose) and other modules stay; stats:read is skipped.
    expect(lastEmitted(w)).toEqual([`${G}:loose`, `${G}:rules:manage`, `${G}:rules:read`, 'user:read'])
    expect(ui.toast).toHaveBeenCalledWith(i18n.global.t('roles.restoreSkipped', { n: 1 }, 1), 'info')
    expect(renderGuard([`${G}:rules:read`], { roleKey: 'admin', readOnly: true }).find('[data-testid="module-restore"]').exists()).toBe(false)
  })

  it('disables an unticked permission the editor does not hold, and lets it be unticked', async () => {
    const grantable = (k: string) => k !== `${G}:bypass` && k !== 'user:delete'
    const w = renderGuard([], { grantable })
    expect(box(w, `${G}:bypass`).element.disabled).toBe(true)
    expect(box(w, 'user:delete').element.disabled).toBe(true)
    expect(w.findAll('[data-testid="permission-not-held"]')).toHaveLength(2)
    // A scoped action the editor holds at some scope stays open to it.
    expect(box(renderGuard([], { grantable: () => false, scopeLimit: limitOf([`${G}:rules:manage:own`]) }), `${G}:rules:manage`).element.disabled).toBe(false)
    const held = renderGuard([`${G}:bypass`], { grantable })
    expect(box(held, `${G}:bypass`).element.disabled).toBe(false)
    await box(held, `${G}:bypass`).setValue(false)
    expect(lastEmitted(held)).toEqual([])
  })

  it('hints a ticked permission\'s requirements that are not ticked', () => {
    const w = renderGuard([`${G}:rules:manage:own`])
    expect(w.get('[data-testid="permission-requires"]').text()).toContain(i18n.global.t('roles.requiresHint', { names: 'View rules' }))
    expect(renderGuard([`${G}:rules:manage:own`, `${G}:rules:read`]).find('[data-testid="permission-requires"]').exists()).toBe(false)
  })

  it('opens ScopePicker in plugin mode for a plugin scoped action', () => {
    const picker = renderGuard([`${G}:rules:manage:own`]).getComponent(ScopePicker)
    expect(picker.props('target')).toEqual({ kind: 'plugin', key: 'guard', local: 'rules:manage', scope: { resource: 'ruleset', own: true }, label: 'Manage rules' })
  })

  it('a group or module checkbox leaves critical permissions out; they are ticked one by one (D1)', async () => {
    const w = renderGuard([])
    await w.get('[data-group="ops"] input[data-testid="group-select-all"]').setValue(true)
    expect(lastEmitted(w)).toEqual([`${G}:stats:read`])
    // All the group ticks is ticked: the checkbox reads "all" without bypass.
    const ticked = renderGuard([`${G}:stats:read`])
    expect(ticked.get<HTMLInputElement>('[data-group="ops"] input[data-testid="group-select-all"]').element.checked).toBe(true)
    await box(ticked, `${G}:bypass`).setValue(true)
    expect(lastEmitted(ticked)).toEqual([`${G}:bypass`, `${G}:stats:read`])
  })

  it('a read-only role does not say what the editor cannot grant', () => {
    const grantable = (k: string) => k !== `${G}:bypass`
    expect(renderGuard([], { grantable }).findAll('[data-testid="permission-not-held"]')).toHaveLength(1)
    expect(renderGuard([], { grantable, readOnly: true }).findAll('[data-testid="permission-not-held"]')).toHaveLength(0)
  })

  describe('restoring the suggestion: the cases the matrix answers alike', () => {
    for (const c of RESTORE_CASES) {
      it(c.name, async () => {
        ui.toast.mockClear()
        const m: PermissionModule = {
          module: 'plugin.m',
          label: 'M',
          source: 'plugin',
          plugin_key: 'm',
          status: 'active',
          permissions: [{ ...perm(RESTORE_ACTION), scope_type: 'plugin:flow', scope: { resource: 'flow', own: true }, defaults: { operator: c.want } }]
        }
        const w = mount(PermissionTree, {
          props: { modules: [m], granted: new Set(c.cell), roleKey: 'operator', grantable: (k: string) => permissionIndex(c.editor).has(k) },
          global: { plugins: [i18n], stubs: { EntityPicker: true } }
        })
        const mark = w.find('[data-testid="permission-mark"]')
        expect(mark.exists() ? mark.attributes('data-mark') : null).toBe(c.state === 'differs' ? 'differs' : c.state === 'same' && c.want.length ? 'suggested' : null)
        const restore = w.find('[data-testid="module-restore"]')
        expect(restore.exists()).toBe(c.state === 'differs')
        if (restore.exists()) {
          await restore.trigger('click')
          expect(lastEmitted(w)).toEqual(c.restored)
        }
      })
    }
  })

  it('says a disabled plugin\'s grants are kept', () => {
    const w = renderGuard([], {})
    expect(w.text()).not.toContain(i18n.global.t('roles.pluginDisabledKept'))
    const off = mount(PermissionTree, { props: { modules: [{ ...guard, status: 'disabled' }], granted: new Set<string>() }, global: { plugins: [i18n] } })
    expect(off.text()).toContain(i18n.global.t('roles.pluginDisabledKept'))
  })
})

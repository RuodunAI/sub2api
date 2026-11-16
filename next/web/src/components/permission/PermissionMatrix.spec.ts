import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { i18n } from '@/i18n'
import { ALL_SCOPE, OWN_SCOPE, permissionIndex, refScope, type Scope } from '@/authz/implies'
import type { RoleKeys } from '@/authz/pluginGrants'
import { RESTORE_ACTION, RESTORE_CASES } from '@/authz/testing/restoreCases'
import PermissionMatrix from './PermissionMatrix.vue'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from './useGrantMatrix'

enableAutoUnmount(afterEach)
const ui = vi.hoisted(() => ({ toast: vi.fn() }))
vi.mock('@sub2api/ui', async (original) => ({ ...(await original<object>()), toast: ui.toast }))

const READ = 'plugin.m:flow:read'
const MANAGE = 'plugin.m:flow:manage'
const EXEMPT = 'plugin.m:exempt'
const perm = (action: string, over: Partial<MatrixPerm> = {}): MatrixPerm => ({ action, local: action.slice('plugin.m:'.length), label: action, risk: 'low', status: 'active', ...over })
const PERMS: MatrixPerm[] = [
  perm(READ, { group: 'flow', defaults: { admin: [READ], operator: [READ] } }),
  perm(MANAGE, { group: 'flow', risk: 'sensitive', scope: { resource: 'flow', own: true }, defaults: { admin: [MANAGE], operator: [`${MANAGE}:own`] } }),
  perm(EXEMPT, { risk: 'critical' })
]
const role = (key: string, over: Partial<MatrixRole> = {}): MatrixRole => ({ key, name: key, builtin: true, superuser: false, ...over })
const ROLES = [role('super_admin', { superuser: true }), role('admin'), role('operator'), role('cs', { builtin: false })]
const SUGGESTED: RoleKeys = { admin: [MANAGE, READ], operator: [`${MANAGE}:own`, READ], cs: [] }

interface Setup {
  baseline?: RoleKeys
  lockOf?: (p: MatrixPerm, r: MatrixRole) => CellLock
  scopeLimit?: (action: string) => Scope[]
  perms?: MatrixPerm[]
  readOnly?: boolean
  props?: Record<string, unknown>
}
function render({ baseline = SUGGESTED, lockOf = (_p, r) => (r.superuser ? 'superuser' : null), scopeLimit = () => [ALL_SCOPE], perms = PERMS, readOnly = false, props = {} }: Setup = {}) {
  const base = ref(baseline)
  const matrix = useGrantMatrix({ perms: ref(perms), roles: ref(ROLES), baseline: base, lockOf, scopeLimit, readOnly: ref(readOnly) })
  const w = mount(PermissionMatrix, {
    props: { matrix, groups: [{ key: 'flow', label: 'Flows', sort: 10 }], pluginKey: 'm', ...props },
    attachTo: document.body,
    global: {
      plugins: [i18n],
      stubs: { ScopePicker: { name: 'ScopePicker', props: ['action', 'scopes', 'offer', 'target', 'disabled'], emits: ['update:scopes'], template: '<div data-testid="scope-picker" />' } },
      config: {
        warnHandler: (msg) => {
          if (msg.includes('Failed to resolve component')) throw new Error(msg)
        }
      }
    }
  })
  return { w, matrix, base }
}
type W = ReturnType<typeof render>['w']
const cell = (w: W, action: string, roleKey: string) => w.get(`tr[data-permission="${action}"] td[data-role="${roleKey}"]`)
const box = (w: W, action: string, roleKey: string) => cell(w, action, roleKey).get<HTMLInputElement>('input[type="checkbox"]')
const select = (w: W, action: string, roleKey: string) => cell(w, action, roleKey).get<HTMLSelectElement>('select')
const optionTexts = (w: W, action: string, roleKey: string) => select(w, action, roleKey).findAll('option').map((o) => o.text())
async function choose(w: W, action: string, roleKey: string, text: string) {
  const i = optionTexts(w, action, roleKey).indexOf(text)
  expect(i, text).toBeGreaterThanOrEqual(0)
  await select(w, action, roleKey).setValue(String(i))
}

describe('PermissionMatrix', () => {
  it('starts at the suggested values and marks cells changed from them', async () => {
    const { w, matrix } = render()
    expect(box(w, READ, 'operator').element.checked).toBe(true)
    expect(select(w, MANAGE, 'operator').element.selectedOptions[0].text).toBe('Created by them')
    expect(w.find('[data-testid="cell-differs"]').exists()).toBe(false)

    await box(w, READ, 'operator').setValue(false)
    await choose(w, MANAGE, 'admin', 'Created by them')
    expect(cell(w, READ, 'operator').find('[data-testid="cell-differs"]').exists()).toBe(true)
    expect(w.findAll('[data-testid="cell-differs"]')).toHaveLength(2)
    expect(matrix.changes.value).toHaveLength(2)

    // The row's restore puts back that row only.
    await w.get(`tr[data-permission="${READ}"] [data-testid="matrix-restore-row"]`).trigger('click')
    expect(box(w, READ, 'operator').element.checked).toBe(true)
    expect(matrix.value.value.admin).toEqual([`${MANAGE}:own`, READ])
  })

  it('restore suggested skips locked cells', async () => {
    const lockOf = (_p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : r.key === 'operator' ? 'role_readonly' : null)
    const { w, matrix } = render({ baseline: { admin: [], operator: [] }, lockOf })
    await w.get('[data-testid="matrix-restore"]').trigger('click')
    expect(matrix.value.value).toEqual({ admin: [MANAGE, READ], operator: [] })
    expect(box(w, READ, 'operator').element.disabled).toBe(true)
  })

  it('super admin column is ticked and fixed', () => {
    const { w } = render({ baseline: {} })
    expect(box(w, READ, 'super_admin').element.checked).toBe(true)
    expect(box(w, READ, 'super_admin').element.disabled).toBe(true)
    expect(select(w, MANAGE, 'super_admin').element.disabled).toBe(true)
    expect(select(w, MANAGE, 'super_admin').element.selectedOptions[0].text).toBe('All')
  })

  it('pending_review cells show what is held (the baseline) read-only with the badge', () => {
    const lockOf = (_p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : r.key === 'admin' ? 'pending_review' : null)
    const { w } = render({ baseline: { admin: [`${MANAGE}:own`], operator: [] }, lockOf })
    // Held is what the cell shows, not the plugin's suggestion (admin: all).
    expect(select(w, MANAGE, 'admin').element.selectedOptions[0].text).toBe('Created by them')
    expect(box(w, READ, 'admin').element.checked).toBe(false)
    expect(box(w, READ, 'admin').element.disabled).toBe(true)
    expect(cell(w, READ, 'admin').text()).toContain('Awaiting super admin')
    expect(cell(w, READ, 'admin').find('[data-testid="cell-differs"]').exists()).toBe(false)
  })

  it('a locked control carries its reason in its label; the differs dot is labelled', async () => {
    const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : p.risk === 'critical' ? 'critical' : null)
    const { w } = render({ lockOf })
    expect(box(w, EXEMPT, 'admin').attributes('aria-label')).toBe(`${EXEMPT} · admin · Only a super admin can grant this`)
    expect(box(w, READ, 'admin').attributes('aria-label')).toBe(`${READ} · admin`)
    await box(w, READ, 'admin').setValue(false)
    const dot = cell(w, READ, 'admin').get('[data-testid="cell-differs"]')
    expect(dot.attributes('role')).toBe('img')
    expect(dot.attributes('aria-label')).toBe(i18n.global.t('roles.differs'))
  })

  it('the suggestion speaks for built-in roles only: custom roles never differ and restore keeps their grants', async () => {
    const { w, matrix } = render({ baseline: { ...SUGGESTED, cs: [READ] } })
    expect(cell(w, READ, 'cs').find('[data-testid="cell-differs"]').exists()).toBe(false)
    expect(w.find('[data-testid="matrix-differs"]').exists()).toBe(false)

    await box(w, READ, 'admin').setValue(false)
    expect(w.get('[data-testid="matrix-differs"]').text()).toContain('1')
    await w.get('[data-testid="matrix-restore"]').trigger('click')
    expect(matrix.value.value.admin).toEqual([MANAGE, READ])
    expect(matrix.value.value.cs).toEqual([READ])
    await w.get(`tr[data-permission="${READ}"] td[data-role="cs"] input`).setValue(false)
    expect(w.find(`tr[data-permission="${READ}"] [data-testid="matrix-restore-row"]`).exists()).toBe(false)
  })

  it('critical row is disabled for a non-super editor', () => {
    const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : p.risk === 'critical' ? 'critical' : null)
    const { w } = render({ lockOf })
    expect(box(w, EXEMPT, 'admin').element.disabled).toBe(true)
    expect(box(w, EXEMPT, 'super_admin').element.checked).toBe(true)
    expect(w.get(`tr[data-permission="${EXEMPT}"] th`).text()).toContain('Only a super admin can grant this')
  })

  it('scoped cell offers only what scopeLimit implies, and "custom" opens ScopePicker below the table', async () => {
    const { w, matrix } = render({ scopeLimit: () => [OWN_SCOPE, refScope('flow/2')] })
    expect(optionTexts(w, MANAGE, 'cs')).toEqual(['Not granted', 'Created by them', 'Chosen…'])
    expect(w.find('[data-testid="scope-picker"]').exists()).toBe(false)

    await choose(w, MANAGE, 'cs', 'Chosen…')
    await w.vm.$nextTick()
    const panel = w.get('[data-testid="matrix-scope-panel"]')
    expect(w.get('table').element.compareDocumentPosition(panel.element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    // The select says what is being chosen while the panel is open, and the panel takes the focus.
    expect(select(w, MANAGE, 'cs').element.selectedOptions[0].text).toBe('Chosen…')
    expect(document.activeElement).toBe(panel.element)
    const picker = w.getComponent({ name: 'ScopePicker' })
    expect(picker.props('target')).toEqual({ kind: 'plugin', key: 'm', local: 'flow:manage', scope: { resource: 'flow', own: true }, label: MANAGE })
    expect(picker.props('offer').refs).toEqual(['flow/2'])

    picker.vm.$emit('update:scopes', [refScope('flow/2')])
    await w.vm.$nextTick()
    expect(matrix.value.value.cs).toEqual([`${MANAGE}:flow/2`])
    expect(select(w, MANAGE, 'cs').element.selectedOptions[0].text).toBe('1 chosen')
  })

  it('group checkbox ticks every editable row of the group as concrete keys (D1)', async () => {
    const { w, matrix } = render({ baseline: {}, scopeLimit: () => [OWN_SCOPE] })
    await w.get('tr[data-group="flow"] input[data-group-role="cs"]').setValue(true)
    expect(matrix.value.value.cs).toEqual([`${MANAGE}:own`, READ])
    expect(matrix.value.value.cs.some((k) => k.endsWith(':*'))).toBe(false)
    expect(w.get<HTMLInputElement>('tr[data-group="flow"] input[data-group-role="cs"]').element.checked).toBe(true)
    expect(w.get('tr[data-group="flow"] input[data-group-role="cs"]').attributes('title')).toBeUndefined()

    await w.get('tr[data-group="flow"] input[data-group-role="cs"]').setValue(false)
    expect(matrix.value.value.cs).toEqual([])
  })

  it('group checkbox goes by the shown rows only: folded rows are neither counted nor cleared', async () => {
    const { w, matrix } = render({ baseline: { cs: [READ] }, props: { foldAfter: 1 } })
    const group = () => w.get<HTMLInputElement>('tr[data-group="flow"] input[data-group-role="cs"]')
    // READ (shown) is granted, MANAGE (folded) is not: the shown rows are all ticked.
    expect(group().element.checked).toBe(true)
    await group().setValue(false)
    expect(matrix.value.value.cs).toEqual([])
    matrix.setCell(PERMS[1], ROLES[3], [MANAGE])
    await group().setValue(true)
    expect(matrix.value.value.cs).toEqual([MANAGE, READ])
  })

  it('group checkbox leaves rows the editor may not tick alone (not held, no scope to offer)', async () => {
    // The editor holds READ only: MANAGE is not_held, granted to cs by someone else.
    const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : p.action === MANAGE ? 'not_held' : null)
    const { w, matrix } = render({ baseline: { cs: [READ, MANAGE] }, lockOf })
    const group = () => w.get<HTMLInputElement>('tr[data-group="flow"] input[data-group-role="cs"]')
    expect(group().element.checked).toBe(true)
    await group().setValue(false)
    expect(matrix.value.value.cs).toEqual([MANAGE])

    // A scoped row with no scope the editor can offer is not counted either: the group can still be cleared.
    const { w: w2, matrix: m2 } = render({ baseline: { cs: [READ, `${MANAGE}:flow/3`] }, scopeLimit: (a) => (a === MANAGE ? [refScope('flow/3')] : [ALL_SCOPE]) })
    const group2 = w2.get<HTMLInputElement>('tr[data-group="flow"] input[data-group-role="cs"]')
    expect(group2.element.checked).toBe(true)
    await group2.setValue(false)
    expect(m2.value.value.cs).toEqual([`${MANAGE}:flow/3`])
  })

  it('custom roles fold under "more roles" when collapseCustom', async () => {
    const { w } = render({ props: { collapseCustom: true } })
    expect(w.find('th[data-role="cs"]').exists()).toBe(false)
    await w.get('[data-testid="matrix-more-roles"]').trigger('click')
    expect(w.find('th[data-role="cs"]').exists()).toBe(true)
  })

  it('a custom role granted something is not folded away, and the button counts its grants', async () => {
    const { w } = render({ baseline: { ...SUGGESTED, cs: [READ] }, props: { collapseCustom: true } })
    expect(w.find('th[data-role="cs"]').exists()).toBe(true)
    await w.get('[data-testid="matrix-more-roles"]').trigger('click')
    expect(w.find('th[data-role="cs"]').exists()).toBe(false)
    expect(w.get('[data-testid="matrix-more-roles"]').text()).toContain(i18n.global.t('plugins.matrix.moreRolesGrants', { n: 1 }, 1))
  })

  it('a group checkbox leaves a critical row out: it is ticked on its own (D1)', async () => {
    const perms = [perm(READ, { group: 'flow' }), perm(EXEMPT, { group: 'flow', risk: 'critical' })]
    const { w, matrix } = render({ baseline: {}, perms })
    const group = () => w.get<HTMLInputElement>('tr[data-group="flow"] input[data-group-role="admin"]')
    // Its title says so, as no group without a critical row does (N5).
    expect(group().attributes('title')).toBe(i18n.global.t('roles.groupSkipsCritical'))
    await group().setValue(true)
    expect(matrix.value.value.admin).toEqual([READ])
    expect(group().element.checked).toBe(true)
    await box(w, EXEMPT, 'admin').setValue(true)
    expect(matrix.value.value.admin).toEqual([EXEMPT, READ])
  })

  it('keeps the cells changed locally when the baseline is read again (a reload, a conflict)', async () => {
    const { w, matrix, base } = render()
    await box(w, READ, 'operator').setValue(false)
    // Meanwhile someone gave admin the critical row and the operator the own rows only.
    base.value = { ...SUGGESTED, admin: [EXEMPT, MANAGE, READ] }
    await w.vm.$nextTick()
    expect(matrix.value.value.admin).toEqual([EXEMPT, MANAGE, READ])
    expect(matrix.value.value.operator).toEqual([`${MANAGE}:own`])
    expect(matrix.changes.value.map((c) => `${c.role} ${c.action}`)).toEqual([`operator ${READ}`])
  })

  it('a read-only matrix changes nothing', async () => {
    const { w, matrix } = render({ readOnly: true })
    expect(box(w, READ, 'admin').element.disabled).toBe(true)
    matrix.setCell(PERMS[0], ROLES[1], [])
    expect(matrix.dirty.value).toBe(false)
    expect(w.find('[data-testid="matrix-restore"]').exists()).toBe(false)
  })

  it('hides the super admin column on phones and says why', () => {
    const { w } = render()
    expect(w.get('th[data-role="super_admin"]').classes()).toContain('hidden')
    expect(w.get('[data-testid="matrix-super-note"]').classes()).toContain('md:hidden')
  })

  describe('restoring the suggestion: the cases the role editor answers alike', () => {
    for (const c of RESTORE_CASES) {
      it(c.name, async () => {
        ui.toast.mockClear()
        const perms = [perm(RESTORE_ACTION, { scope: { resource: 'flow', own: true }, defaults: { operator: c.want } })]
        const held = permissionIndex(c.editor)
        const { w, matrix } = render({ perms, baseline: { operator: c.cell }, scopeLimit: (a) => held.scopes(a), lockOf: (_p, r) => (r.superuser ? 'superuser' : null) })
        expect(cell(w, RESTORE_ACTION, 'operator').find('[data-testid="cell-differs"]').exists()).toBe(c.state === 'differs')
        expect(matrix.restore()).toBe(c.state === 'unreachable' ? 1 : 0)
        expect(matrix.cell(perms[0], ROLES[2])).toEqual(c.restored)
      })
    }
  })

  it('folds rows after foldAfter', async () => {
    const { w } = render({ props: { foldAfter: 2 } })
    expect(w.find(`tr[data-permission="${EXEMPT}"]`).exists()).toBe(false)
    await w.get('[data-testid="matrix-fold"]').trigger('click')
    expect(w.find(`tr[data-permission="${EXEMPT}"]`).exists()).toBe(true)
  })

  it('a removed row can only be unticked', async () => {
    const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : p.action === READ ? 'removed' : null)
    const { w, matrix } = render({ baseline: { admin: [READ] }, lockOf })
    expect(box(w, READ, 'operator').element.disabled).toBe(true)
    matrix.setCell(PERMS[0], ROLES[2], [READ])
    expect(matrix.value.value.operator ?? []).toEqual([])
    await box(w, READ, 'admin').setValue(false)
    expect(matrix.value.value.admin).toEqual([])
  })
})

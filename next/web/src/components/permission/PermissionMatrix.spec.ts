import { afterEach, describe, expect, it } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { ref } from 'vue'
import { i18n } from '@/i18n'
import { ALL_SCOPE, OWN_SCOPE, refScope, type Scope } from '@/authz/implies'
import type { RoleKeys } from '@/authz/pluginGrants'
import PermissionMatrix from './PermissionMatrix.vue'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from './useGrantMatrix'

enableAutoUnmount(afterEach)

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
  props?: Record<string, unknown>
}
function render({ baseline = SUGGESTED, lockOf = (_p, r) => (r.superuser ? 'superuser' : null), scopeLimit = () => [ALL_SCOPE], props = {} }: Setup = {}) {
  const matrix = useGrantMatrix({ perms: ref(PERMS), roles: ref(ROLES), baseline: ref(baseline), lockOf })
  const w = mount(PermissionMatrix, {
    props: { matrix, perms: PERMS, groups: [{ key: 'flow', label: 'Flows', sort: 10 }], roles: ROLES, pluginKey: 'm', lockOf, scopeLimit, ...props },
    global: {
      plugins: [i18n],
      stubs: { ScopePicker: { name: 'ScopePicker', props: ['action', 'scopes', 'offer', 'plugin', 'resource', 'disabled'], emits: ['update:scopes'], template: '<div data-testid="scope-picker" />' } },
      config: {
        warnHandler: (msg) => {
          if (msg.includes('Failed to resolve component')) throw new Error(msg)
        }
      }
    }
  })
  return { w, matrix }
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

  it('pending_review cells show the suggestion read-only with the badge', () => {
    const lockOf = (_p: MatrixPerm, r: MatrixRole): CellLock => (r.superuser ? 'superuser' : r.key === 'admin' ? 'pending_review' : null)
    const { w } = render({ baseline: { admin: [], operator: [] }, lockOf })
    expect(box(w, READ, 'admin').element.checked).toBe(true)
    expect(box(w, READ, 'admin').element.disabled).toBe(true)
    expect(cell(w, READ, 'admin').text()).toContain('Awaiting super admin')
    expect(cell(w, READ, 'admin').find('[data-testid="cell-differs"]').exists()).toBe(false)
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
    const panel = w.get('[data-testid="matrix-scope-panel"]')
    expect(w.get('table').element.compareDocumentPosition(panel.element) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    const picker = w.getComponent({ name: 'ScopePicker' })
    expect(picker.props('plugin')).toEqual({ key: 'm', local: 'flow:manage', scope: { resource: 'flow', own: true } })
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

    await w.get('tr[data-group="flow"] input[data-group-role="cs"]').setValue(false)
    expect(matrix.value.value.cs).toEqual([])
  })

  it('custom roles fold under "more roles" when collapseCustom', async () => {
    const { w } = render({ props: { collapseCustom: true } })
    expect(w.find('th[data-role="cs"]').exists()).toBe(false)
    await w.get('[data-testid="matrix-more-roles"]').trigger('click')
    expect(w.find('th[data-role="cs"]').exists()).toBe(true)
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

import { describe, expect, it } from 'vitest'
import { ALL_SCOPE, OWN_SCOPE, refScope } from './implies'
import { ALL_SCOPE as ALL, permissionIndex } from './implies'
import { cellKeys, cellOfScopes, cellSummary, diffCells, grantableWithin, isSuggestedRole, keysFromSuffixes, localOf, narrows, newRiskyGrants, replayCells, restoreCell, riskSummary, sameKeys, scopesOfCell, suggestedOf, suggestionState, toManifestGrant, withCell } from './pluginGrants'
import { RESTORE_CASES } from './testing/restoreCases'

const A = 'plugin.m:flow:manage'
const B = 'plugin.m:flow:read'

describe('plugin grants', () => {
  it('the suggestion speaks for the built-in roles but the super admin', () => {
    expect(['admin', 'operator', 'supplier', 'user'].every(isSuggestedRole)).toBe(true)
    expect(isSuggestedRole('super_admin')).toBe(false)
    expect(isSuggestedRole('auditor')).toBe(false)
  })

  it('localOf takes the plugin-local part of a full key', () => {
    expect(localOf(A)).toBe('flow:manage')
    expect(localOf(`${A}:own`)).toBe('flow:manage:own')
  })

  it('cellKeys/withCell replace one action only, canonical and sorted', () => {
    const keys = [B, `${A}:own`, `${A}:flow/3`]
    expect(cellKeys(keys, A)).toEqual([`${A}:own`, `${A}:flow/3`])
    expect(withCell(keys, A, [`${A}:*`])).toEqual([A, B])
    expect(withCell([`${B}:*`, A], A, [])).toEqual([B])
    // A narrower scope another one implies is not kept beside it.
    expect(withCell([], A, [A, `${A}:own`])).toEqual([A])
  })

  it('scopesOfCell and cellOfScopes convert both ways', () => {
    expect(scopesOfCell([`${A}:own`, `${A}:value/input`, B], A)).toEqual([OWN_SCOPE, refScope('value/input')])
    expect(cellOfScopes(A, [refScope('flow/3'), OWN_SCOPE])).toEqual([`${A}:flow/3`, `${A}:own`])
    expect(cellOfScopes(A, [ALL_SCOPE])).toEqual([A])
  })

  it('sameKeys ignores spelling and order', () => {
    expect(sameKeys([`${A}:*`], [A])).toBe(true)
    expect(sameKeys([`${A}:own`, B], [B, `${A}:own`])).toBe(true)
    expect(sameKeys([`${A}:own`], [A])).toBe(false)
  })

  it('narrows only takes away', () => {
    expect(narrows([A], [`${A}:own`], A)).toBe(true)
    expect(narrows([`${A}:flow/1`, `${A}:flow/2`], [`${A}:flow/2`], A)).toBe(true)
    expect(narrows([`${A}:own`], [A], A)).toBe(false)
    expect(narrows([], [`${A}:own`], A)).toBe(false)
  })

  it('suggestedOf merges defaults per role', () => {
    const perms = [{ defaults: { admin: [A], operator: [`${A}:own`] } }, { defaults: { operator: [B] } }, {}]
    expect(suggestedOf(perms)).toEqual({ admin: [A], operator: [`${A}:own`, B].sort() })
  })

  it('diffCells reports one change per cell, spelling aside', () => {
    const base = { operator: [`${A}:*`, B], user: [] }
    const next = { operator: [A], user: [B] }
    expect(diffCells(base, next, [A, B], ['operator', 'user'])).toEqual([
      { action: B, role: 'operator', from: [B], to: [] },
      { action: B, role: 'user', from: [], to: [B] }
    ])
  })

  it('riskSummary counts the permissions of a risk and the roles given one', () => {
    const perms = [{ action: A, risk: 'sensitive' as const }, { action: B, risk: 'low' as const }]
    expect(riskSummary({ admin: [A], operator: [`${A}:own`, B], user: [B] }, perms, 'sensitive')).toEqual({ perms: 1, roles: ['admin', 'operator'] })
    expect(riskSummary({ user: [B] }, perms, 'sensitive')).toEqual({ perms: 0, roles: [] })
  })

  it('keysFromSuffixes maps "" to the bare action', () => {
    expect(keysFromSuffixes(A, ['', 'own'])).toEqual([A, `${A}:own`])
    expect(keysFromSuffixes(A, ['value/input'])).toEqual([`${A}:value/input`])
  })

  it('toManifestGrant: unscoped → true, scoped all → ["*"], value/own/id kept as suffixes, none → null', () => {
    const scoped = { action: A, scope: { resource: 'flow', own: true } }
    expect(toManifestGrant({ action: B }, [B])).toBe(true)
    expect(toManifestGrant(scoped, [`${A}:*`])).toEqual(['*'])
    expect(toManifestGrant(scoped, [`${A}:value/input`, `${A}:own`, `${A}:flow/3`])).toEqual(['flow/3', 'own', 'value/input'])
    expect(toManifestGrant(scoped, [B])).toBeNull()
  })

  it('newRiskyGrants lists the sensitive and critical cells a change grants more of, critical first', () => {
    const C = 'plugin.m:bypass'
    const perms = [{ action: A, risk: 'sensitive' as const }, { action: B, risk: 'low' as const }, { action: C, risk: 'critical' as const }]
    const base = { admin: [`${A}:own`], operator: [A] }
    expect(newRiskyGrants(base, { admin: [A, B, C], operator: [`${A}:own`] }, perms)).toEqual([
      { action: C, role: 'admin', risk: 'critical' },
      { action: A, role: 'admin', risk: 'sensitive' }
    ])
    // Taking away and keeping ask nothing.
    expect(newRiskyGrants(base, { admin: [`${A}:own`], operator: [] }, perms)).toEqual([])
  })

  it('grantableWithin checks a key against the editor\'s scopes of its action', () => {
    const may = grantableWithin((a) => (a === A ? [OWN_SCOPE] : a === B ? [ALL] : []))
    expect(may(`${A}:own`)).toBe(true)
    expect(may(A)).toBe(false)
    expect(may(`${B}:own`)).toBe(true)
    expect(may('plugin.m:other')).toBe(false)
  })

  it('cellSummary names a cell\'s choice', () => {
    expect(cellSummary([], A)).toEqual({ choice: 'none', refs: 0 })
    expect(cellSummary([A, `${A}:own`], A)).toEqual({ choice: 'all', refs: 0 })
    expect(cellSummary([`${A}:own`], A)).toEqual({ choice: 'own', refs: 0 })
    expect(cellSummary([`${A}:own`, `${A}:flow/3`], A)).toEqual({ choice: 'custom', refs: 1 })
  })

  describe('restoring the suggestion (shared with the role editor and the matrix)', () => {
    for (const c of RESTORE_CASES) {
      it(c.name, () => {
        const mayGrant = (k: string) => permissionIndex(c.editor).has(k)
        expect(suggestionState(c.cell, c.want, mayGrant)).toBe(c.state)
        expect(restoreCell(c.cell, c.want, mayGrant) ?? c.cell).toEqual(c.restored)
      })
    }
  })
  describe('replayCells', () => {
    const A = 'plugin.m:flow:manage'
    const B = 'plugin.m:flow:read'
    it('puts the edited cells on top of the fresh grants, keeping what others changed elsewhere', () => {
      const r = replayCells({ op: [B] }, { op: [B, `${A}:own`] }, { op: [B], lead: [B] }, [A, B], ['op', 'lead'])
      expect(r.value).toEqual({ op: [`${A}:own`, B], lead: [B] })
      expect(r.kept.map((c) => c.action)).toEqual([A])
      expect(r.dropped).toEqual([])
      expect(r.clashes).toEqual([])
    })
    it('a cell both sides changed keeps the edit and is a clash; the same change on both sides is none', () => {
      const r = replayCells({ op: [] }, { op: [`${A}:own`], lead: [B] }, { op: [A], lead: [B] }, [A, B], ['op', 'lead'])
      expect(r.value.op).toEqual([`${A}:own`])
      expect(r.clashes.map((c) => [c.role, c.action])).toEqual([['op', A]])
    })
    it('drops an edit the cell may no longer take', () => {
      const r = replayCells({ op: [] }, { op: [A, B] }, { op: [] }, [A, B], ['op'], (c) => c.action !== A)
      expect(r.value.op).toEqual([B])
      expect(r.dropped.map((c) => c.action)).toEqual([A])
    })
  })
})

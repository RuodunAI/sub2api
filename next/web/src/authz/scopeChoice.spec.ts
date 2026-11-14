import { describe, expect, it } from 'vitest'
import { parsePermission, patternKey, type Scope } from './implies'
import { choiceOf, defaultScopes, offerOf, scopesOf, withScopes } from './scopeChoice'

const s = (suffix: string): Scope => parsePermission(suffix ? `account:update:${suffix}` : 'account:update')!.scope
const keys = (scopes: Scope[]) => scopes.map((scope) => patternKey({ action: 'account:update', scope }))

describe('choiceOf / scopesOf', () => {
  it('splits scopes into the editor choices and back', () => {
    const scopes = [s('own'), s('group/12'), s('account/5'), s('group/3/account/9'), s('group/*')]
    const c = choiceOf(scopes)
    expect(c).toMatchObject({ all: false, own: true, groups: [12], ids: [5], pairs: [[3, 9]] })
    expect(keys(c.other)).toEqual(['account:update:group/*'])
    // group/* implies group/12 and group/3/account/9: they fold into it.
    expect(keys(scopesOf(c))).toEqual(['account:update:own', 'account:update:account/5', 'account:update:group/*'])
  })

  it('keeps only every row once it is chosen', () => {
    expect(keys(scopesOf({ ...choiceOf([s('own'), s('group/1')]), all: true }))).toEqual(['account:update'])
  })
})

describe('granted keys', () => {
  it('replaces the scopes of one action', () => {
    const granted = new Set(['account:update:own', 'account:update:group/1', 'user:read', 'account:read'])
    expect([...withScopes(granted, 'account:update', [s('account/7')])].sort()).toEqual(['account:read', 'account:update:account/7', 'user:read'])
    expect([...withScopes(granted, 'account:update', [])].sort()).toEqual(['account:read', 'user:read'])
  })
})

describe('offerOf', () => {
  it('offers everything to a holder of every row', () => {
    const o = offerOf([s('')])
    expect(o).toEqual({ all: true, own: true, groups: 'any', ids: 'any', refs: 'any' })
    expect(keys(defaultScopes(o))).toEqual(['account:update'])
  })

  it('offers no more than the holder\'s own scopes', () => {
    const o = offerOf([s('own'), s('group/12'), s('group/13'), s('account/5'), s('group/3/account/9')])
    expect(o).toEqual({ all: false, own: true, groups: [12, 13], ids: [5], refs: null })
    expect(keys(defaultScopes(o))).toEqual(['account:update:own'])
    expect(offerOf([s('group/*')])).toEqual({ all: false, own: false, groups: 'any', ids: null, refs: null })
    expect(defaultScopes(offerOf([s('group/12')]))).toEqual([])
    expect(offerOf([])).toEqual({ all: false, own: false, groups: null, ids: null, refs: null })
    // account:create names no rows.
    expect(offerOf([s('')], 'account:create')).toEqual({ all: true, own: true, groups: 'any', ids: null, refs: 'any' })
  })
})

describe('plugin scopes', () => {
  const action = 'plugin.m:flow:manage'
  const p = (suffix: string): Scope => parsePermission(suffix ? `${action}:${suffix}` : action)!.scope
  const pkeys = (scopes: Scope[]) => scopes.map((scope) => patternKey({ action, scope }))

  it('choiceOf / scopesOf round-trip refs', () => {
    const c = choiceOf([p('own'), p('flow/3'), p('flow/5')])
    expect(c).toMatchObject({ all: false, own: true, groups: [], ids: [], refs: ['flow/3', 'flow/5'], other: [] })
    expect(pkeys(scopesOf(c))).toEqual([`${action}:own`, `${action}:flow/3`, `${action}:flow/5`])
    expect(pkeys(scopesOf({ ...c, all: true }))).toEqual([action])
  })

  it('offerOf offers refs any for all, held refs otherwise', () => {
    expect(offerOf([p('')], action).refs).toBe('any')
    expect(offerOf([p('own'), p('flow/3'), p('flow/5')], action)).toEqual({ all: false, own: true, groups: null, ids: null, refs: ['flow/3', 'flow/5'] })
    expect(offerOf([p('own')], action).refs).toBeNull()
  })
})

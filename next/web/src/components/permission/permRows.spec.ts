import { describe, expect, it } from 'vitest'
import { i18n } from '@/i18n'
import { groupSections, permHint } from './permRows'

describe('permission rows', () => {
  it('groupSections follows the groups, rows of no listed group last without a heading, empty groups left out', () => {
    i18n.global.locale.value = 'en'
    const perms = [{ key: 'a', group: 'ops' }, { key: 'b' }, { key: 'c', group: 'rules' }, { key: 'd', group: 'gone' }]
    const groups = [{ key: 'rules', label: { en: 'Rules', zh: '规则' }, sort: 1 }, { key: 'ops', label: '', sort: 2 }, { key: 'empty', label: 'Empty', sort: 3 }]
    expect(groupSections(perms, groups).map((s) => [s.key, s.label, s.perms.map((p) => p.key)])).toEqual([
      ['rules', 'Rules', ['c']],
      ['ops', 'ops', ['a']],
      ['', '', ['b', 'd']]
    ])
    expect(groupSections(perms).map((s) => s.key)).toEqual([''])
  })

  it('permHint is the key, then what it allows', () => {
    expect(permHint('plugin.m:a', { en: 'Reads A', zh: '读取 A' })).toBe('plugin.m:a\nReads A')
    expect(permHint('plugin.m:a')).toBe('plugin.m:a')
  })
})

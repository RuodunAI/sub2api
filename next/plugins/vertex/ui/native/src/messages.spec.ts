/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import messages from './messages'

const keys = (o: unknown, prefix = ''): string[] =>
  o && typeof o === 'object' ? Object.entries(o).flatMap(([k, v]) => keys(v, prefix ? `${prefix}.${k}` : k)) : [prefix]
const leaves = (o: unknown): string[] => (o && typeof o === 'object' ? Object.values(o).flatMap(leaves) : [String(o)])

describe('messages', () => {
  it('zh and en have the same keys', () => {
    expect(keys(messages.zh)).toEqual(keys(messages.en))
  })

  it('keeps vue-i18n special characters out of plain text', () => {
    for (const lang of [messages.zh, messages.en]) expect(leaves(lang).filter((s) => /[@|{}]/.test(s))).toEqual([])
  })

  it('has no key the forms never use', () => {
    // Looked up with the account type in the key: t(`keyFile.${type}.label`), t(`project.help.${type}`).
    const dynamic = ['keyFile.', 'project.help.']
    const files = import.meta.glob<string>(['./*.vue', './*.ts', '!./*.spec.ts', '!./*.testkit.ts', '!./*.cases.ts', '!./messages.ts'], { query: '?raw', import: 'default', eager: true })
    const sources = Object.values(files).join('\n')
    const unused = keys(messages.en).filter((k) => !dynamic.some((d) => k.startsWith(d)) && !sources.includes(`'${k}'`))
    expect(unused).toEqual([])
  })

  it('names every key file type', () => {
    for (const lang of [messages.zh, messages.en]) {
      expect(Object.keys(lang.keyFile)).toEqual(['vertex_sa', 'vertex_authorized_user'])
      expect(Object.keys(lang.project.help)).toEqual(['vertex_sa', 'vertex_authorized_user'])
    }
  })
})

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

  it('has no key the form never uses', () => {
    // Looked up with the account type as suffix: t(`accessKeyId.help.${type}`), t(`secretAccessKey.help.${type}`).
    const dynamic = ['accessKeyId.help.', 'secretAccessKey.help.']
    const files = import.meta.glob<string>(['./*.vue', './*.ts', '!./*.spec.ts', '!./*.testkit.ts', '!./*.cases.ts', '!./messages.ts'], { query: '?raw', import: 'default', eager: true })
    const sources = Object.values(files).join('\n')
    const unused = keys(messages.en).filter((k) => !dynamic.some((d) => k.startsWith(d)) && !sources.includes(`'${k}'`))
    expect(unused).toEqual([])
  })
})

// What the form tells the administrator (the account types' design,
// docs/PLUGIN-CLOUD-PROVIDERS.md §5).
describe('the form text', () => {
  const both = (pick: (m: typeof messages.en) => string) => [pick(messages.en), pick(messages.zh)]

  it('says when to pick each inference profile prefix', () => {
    for (const help of both((m) => m.prefix.help)) for (const part of ['global', 'Opus 4.1', 'us-gov.', 'ARN']) expect(help).toContain(part)
  })

  it('says what happens to a request body, with no limitation left', () => {
    for (const note of both((m) => m.regionSection.description)) {
      for (const part of ['Claude Code', 'custom', 'cache_control', 'thinking', 'computer use', 'tool search', 'output_format', 'metadata']) expect(note).toContain(part)
      for (const part of ['limitation', '限制', 'parameter override', '参数覆盖']) expect(note).not.toContain(part)
    }
  })

  it('says the China and isolated regions are not served', () => {
    for (const help of both((m) => m.region.help)) for (const part of ['cn-*', 'us-iso-*']) expect(help).toContain(part)
  })

  it('says nothing renews temporary credentials', () => {
    expect(messages.en.sessionExpiresAt.help).toContain('not renewed')
    expect(messages.zh.sessionExpiresAt.help).toContain('不会自动续期')
  })
})

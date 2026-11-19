import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h, ref, type Component } from 'vue'
import { provideHost, type HostContext } from '@sub2api/host'
import { SECRET_MASK } from '@sub2api/ui'
import { forms } from './forms'
import messages from './messages'

type M = Record<string, unknown>
type AccountType = { plugin_key: string; type: string; [key: string]: unknown }

/** This plugin's English messages by dotted key, as host.t would answer. */
const t = (key: string): string => key.split('.').reduce<any>((o, k) => o?.[k], messages.en) ?? key
const { AzureV1Form, AzureDeploymentForm } = forms(t)

/** A console granting `granted`; its own translation answers the key. */
function fakeHost(granted: string[] = []) {
  provideHost({
    permissions: { superuser: () => false, has: (k: string) => granted.includes(k), any: (...k: string[]) => k.some((x) => granted.includes(x)) },
    i18n: { t: (k: string) => k }
  } as unknown as HostContext)
}

const mounted: VueWrapper[] = []
beforeEach(() => fakeHost())
afterEach(() => {
  mounted.splice(0).forEach((w) => w.unmount())
  document.body.innerHTML = ''
})

/** Mounts a form the way the account editor does: v-model credentials and the §23.5 props. */
function editor(form: Component, accountType: AccountType, opts: { stored?: M; errors?: Record<string, string> } = {}) {
  const credentials = ref<M>(opts.stored ? { ...opts.stored } : {})
  const inner = ref<{ validate: () => Promise<boolean> }>()
  const w = mount(
    defineComponent({
      setup: () => () =>
        h('form', [
          h(form, {
            ref: inner,
            modelValue: credentials.value,
            'onUpdate:modelValue': (v: M) => (credentials.value = v),
            mode: opts.stored ? 'edit' : 'create',
            account: opts.stored ? { id: 1, type: accountType.type, credentials: opts.stored } : null,
            accountType,
            errors: opts.errors,
            proxyTicket: async () => 'ticket'
          })
        ])
    }),
    { attachTo: document.body }
  )
  mounted.push(w)
  return { w, credentials, validate: () => inner.value!.validate() }
}

/** The control of each field: a selector and the index among its matches. */
const CONTROL: Record<string, [string, number]> = {
  api_key: ['input[type="password"]', 0],
  base_url: ['input[type="url"]', 0],
  api_version: ['input[type="text"]', 0]
}
const control = (w: VueWrapper, key: string) => w.findAll(CONTROL[key][0])[CONTROL[key][1]]

async function type(w: VueWrapper, key: string, value: string) {
  const el = control(w, key)
  await el.setValue(value)
  await el.trigger('blur')
  await flushPromises()
}

interface Case {
  name: string
  /** Edit an account holding these credentials (create when absent). */
  stored?: M
  /** Typed into the field's control, in order. */
  input?: Record<string, string>
  /** The credentials the editor saves. */
  out: M
  /** validate()'s verdict. */
  ok: boolean
}

/** Types the case's input; '' clears a field (typed over a value first, except the key, whose empty box keeps a stored key). */
async function run(form: Component, accountType: AccountType, c: Case) {
  const { w, credentials, validate } = editor(form, accountType, { stored: c.stored })
  await flushPromises()
  for (const [key, value] of Object.entries(c.input ?? {})) {
    if (value === '' && key !== 'api_key') await type(w, key, 'x')
    await type(w, key, value)
  }
  return { value: JSON.parse(JSON.stringify(credentials.value)), ok: await validate() }
}

/**
 * The api_key and base_url cases every form shares, with `key` a valid key,
 * `url` a valid base URL, `fill` the other fields of a valid account (the
 * defaults a new one starts with) and `required` whether base_url is.
 */
function keyAndUrlCases(key: string, url: string, required: boolean, fill: M = {}): Case[] {
  const typed = (k: string, out: M = { api_key: k }) => ({ input: { api_key: k, base_url: url }, out: { ...fill, base_url: url, ...out } })
  const withUrl = (u: string) => ({ input: { api_key: key, base_url: u }, out: { ...fill, api_key: key, base_url: u } })
  const stored = { ...fill, api_key: SECRET_MASK, base_url: url }
  return [
    { name: 'key only', input: { api_key: key }, out: { ...fill, api_key: key }, ok: !required },
    { name: 'key and base URL', ...withUrl(url), ok: true },
    { name: 'a cleared base URL is not sent', input: { api_key: key, base_url: '' }, out: { ...fill, api_key: key }, ok: !required },
    { name: 'nothing typed', out: { ...fill }, ok: false },
    { name: 'empty key', ...typed(''), ok: false },
    { name: 'key of 7 characters', ...typed('k'.repeat(7)), ok: false },
    { name: 'key of 8 characters', ...typed('k'.repeat(8)), ok: true },
    { name: 'key of 512 characters', ...typed('k'.repeat(512)), ok: true },
    { name: 'key of 513 characters', ...typed('k'.repeat(513)), ok: false },
    { name: 'key with a space', ...typed('sk-abc def'), ok: false },
    { name: 'key with a leading space', ...typed(' sk-abcdef'), ok: false },
    { name: 'key not ASCII', ...typed('sk-ключ-abcdef'), ok: false },
    { name: 'base URL with a query', ...withUrl('https://x.example.com/?a=1'), ok: false },
    { name: 'base URL with a fragment', ...withUrl('https://x.example.com/#a'), ok: false },
    { name: 'base URL with a space', ...withUrl('https://x.example.com/a b'), ok: false },
    { name: 'base URL not http', ...withUrl('ftp://x.example.com'), ok: false },
    { name: 'base URL without a host', ...withUrl('https://'), ok: false },
    { name: 'edit: untouched', stored, out: stored, ok: true },
    { name: 'edit: the emptied key box keeps the stored key', stored, input: { api_key: '' }, out: stored, ok: true },
    { name: 'edit: a new key replaces it', stored, input: { api_key: key }, out: { ...stored, api_key: key }, ok: true },
    { name: 'edit: a short new key', stored, input: { api_key: 'short' }, out: { ...stored, api_key: 'short' }, ok: false },
    { name: 'edit: a stored base URL the rules refuse', stored: { ...stored, base_url: 'ftp://x.example.com' }, out: { ...stored, base_url: 'ftp://x.example.com' }, ok: false },
    { name: 'edit: a key the form does not know goes back as it is', stored: { ...stored, legacy: 'x' }, out: { ...stored, legacy: 'x' }, ok: true }
  ]
}

describe('AzureV1Form', () => {
  const accountType: AccountType = { plugin_key: 'azure-openai', type: 'azure_v1' }
  // The verdicts of the schema form it replaced, compared side by side with
  // it before forms/ was deleted.
  const cases: Case[] = [
    ...keyAndUrlCases('0123456789abcdef', 'https://res.openai.azure.com', true),
    ...['https://res.openai.azure.com/', 'https://res.openai.azure.com/openai', 'https://res.openai.azure.com/openai/v1/', 'https://res.services.ai.azure.com/api/projects/p1'].map((u) => ({
      name: `endpoint ${u}`, input: { api_key: '0123456789abcdef', base_url: u }, out: { api_key: '0123456789abcdef', base_url: u }, ok: true
    })),
    ...['https://res.openai.azure.com/openai/deployments/gpt/chat/completions', 'https://res.openai.azure.com/openai/v1/chat', 'https://user@res.openai.azure.com', 'https://res.openai.azure.com/other'].map((u) => ({
      name: `endpoint ${u}`, input: { api_key: '0123456789abcdef', base_url: u }, out: { api_key: '0123456789abcdef', base_url: u }, ok: false
    }))
  ]
  for (const c of cases) it(c.name, async () => expect(await run(AzureV1Form, accountType, c)).toEqual({ value: c.out, ok: c.ok }))

  it('labels, help and placeholders', async () => {
    const { w } = editor(AzureV1Form, accountType)
    await flushPromises()
    expect(w.findAll('label').map((l) => l.text())).toEqual(['Resource endpoint*', 'API key*'])
    expect(control(w, 'base_url').attributes('placeholder')).toBe(messages.en.baseUrl.placeholder)
    for (const help of [messages.en.apiKey.help, messages.en.baseUrl.helpV1]) expect(w.text()).toContain(help)
  })

  it('a refused endpoint says what to leave out', async () => {
    const { w, validate } = editor(AzureV1Form, accountType)
    await flushPromises()
    await type(w, 'base_url', 'https://res.openai.azure.com/openai/deployments/gpt')
    expect(await validate()).toBe(false)
    expect(w.text()).toContain(messages.en.baseUrl.pattern)
  })

  it('shows a server error at its field', async () => {
    const { w } = editor(AzureV1Form, accountType, { errors: { api_key: 'invalid key' } })
    await flushPromises()
    expect(w.text()).toContain('invalid key')
  })
})

describe('AzureDeploymentForm', () => {
  const accountType: AccountType = { plugin_key: 'azure-openai', type: 'azure_deployment' }
  // The verdicts of the schema form it replaced, compared side by side with
  // it before forms/ was deleted.
  const cases: Case[] = [
    ...keyAndUrlCases('0123456789abcdef', 'https://res.openai.azure.com', true, { api_version: '2024-10-21' }),
    ...['https://res.openai.azure.com/', 'https://res.openai.azure.com/openai', 'https://res.openai.azure.com/openai/v1/', 'https://res.services.ai.azure.com/api/projects/p1'].map((u) => ({
      name: `endpoint ${u}`, input: { api_key: '0123456789abcdef', base_url: u }, out: { ...{ api_version: '2024-10-21' }, api_key: '0123456789abcdef', base_url: u }, ok: true
    })),
    ...['https://res.openai.azure.com/openai/deployments/gpt/chat/completions', 'https://res.openai.azure.com/openai/v1/chat', 'https://user@res.openai.azure.com', 'https://res.openai.azure.com/other'].map((u) => ({
      name: `endpoint ${u}`, input: { api_key: '0123456789abcdef', base_url: u }, out: { ...{ api_version: '2024-10-21' }, api_key: '0123456789abcdef', base_url: u }, ok: false
    })),
    { name: 'a preview API version', input: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '2025-04-01-preview' }, out: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '2025-04-01-preview' }, ok: true },
    { name: 'a cleared API version', input: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '' }, out: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '' }, ok: false },
    { name: 'an API version not a date', input: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: 'latest' }, out: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: 'latest' }, ok: false },
    { name: 'an API version with another suffix', input: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '2024-10-21-beta' }, out: { api_key: '0123456789abcdef', base_url: 'https://res.openai.azure.com', api_version: '2024-10-21-beta' }, ok: false },
    { name: 'edit: an account without an API version gets none filled in', stored: { api_key: SECRET_MASK, base_url: 'https://res.openai.azure.com' }, out: { api_key: SECRET_MASK, base_url: 'https://res.openai.azure.com' }, ok: false }
  ]
  for (const c of cases) it(c.name, async () => expect(await run(AzureDeploymentForm, accountType, c)).toEqual({ value: c.out, ok: c.ok }))

  it('labels, help and placeholders', async () => {
    const { w } = editor(AzureDeploymentForm, accountType)
    await flushPromises()
    expect(w.findAll('label').map((l) => l.text())).toEqual(['Resource endpoint*', 'API key*', 'API version*'])
    expect(control(w, 'base_url').attributes('placeholder')).toBe(messages.en.baseUrl.placeholder)
    for (const help of [messages.en.apiKey.help, messages.en.baseUrl.helpDeployment, messages.en.apiVersion.help]) expect(w.text()).toContain(help)
  })

  it('a new account starts with API version 2024-10-21', async () => {
    const { w, credentials } = editor(AzureDeploymentForm, accountType)
    await flushPromises()
    expect(credentials.value).toEqual({ api_version: '2024-10-21' })
    expect((control(w, 'api_version').element as HTMLInputElement).value).toBe('2024-10-21')
  })

  it('shows a server error at its field', async () => {
    const { w } = editor(AzureDeploymentForm, accountType, { errors: { api_key: 'invalid key' } })
    await flushPromises()
    expect(w.text()).toContain('invalid key')
  })
})

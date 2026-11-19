import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h, ref, type Component } from 'vue'
import { provideHost, type HostContext } from '@sub2api/host'
import { SECRET_MASK } from '@sub2api/ui'
import { forms } from './forms'
import messages from './messages'

type M = Record<string, unknown>

/** This plugin's English messages by dotted key, as host.t would answer. */
const t = (key: string): string => key.split('.').reduce<any>((o, k) => o?.[k], messages.en) ?? key
const { AnthropicKeyForm } = forms(t)

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
function editor(form: Component, opts: { stored?: M; errors?: Record<string, string>; accountType?: M } = {}) {
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
            account: opts.stored ? { id: 1, type: 'apikey', credentials: opts.stored } : null,
            accountType: opts.accountType ?? { plugin_key: 'anthropic', type: 'apikey' },
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

/** The control of each field. */
const CONTROL: Record<string, string> = { api_key: 'input[type="password"]', base_url: 'input[type="url"]' }

async function type(w: VueWrapper, selector: string, value: string) {
  const el = w.find(selector)
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

/** Types the case's input; '' clears a field (typed over a value first, except a secret, whose empty box keeps the stored value). */
async function run(form: Component, c: Case) {
  const { w, credentials, validate } = editor(form, { stored: c.stored })
  await flushPromises()
  for (const [key, value] of Object.entries(c.input ?? {})) {
    if (value === '' && key !== 'api_key') await type(w, CONTROL[key], 'x')
    await type(w, CONTROL[key], value)
  }
  return { value: JSON.parse(JSON.stringify(credentials.value)), ok: await validate() }
}

const KEY = 'sk-ant-api03-abcdef'
// The verdicts of the schema form this replaces (forms/apikey.schema.json:
// api_key required, 8-512 of [!-~]; base_url ^(https?://[^\s?#]+)?$),
// compared side by side with it before it was deleted.
const cases: Case[] = [
  { name: 'key only', input: { api_key: KEY }, out: { api_key: KEY }, ok: true },
  { name: 'key and base URL', input: { api_key: KEY, base_url: 'https://proxy.example.com/anthropic' }, out: { api_key: KEY, base_url: 'https://proxy.example.com/anthropic' }, ok: true },
  { name: 'a cleared base URL is not sent', input: { api_key: KEY, base_url: '' }, out: { api_key: KEY }, ok: true },
  { name: 'no key', out: {}, ok: false },
  { name: 'empty key', input: { api_key: '' }, out: { api_key: '' }, ok: false },
  { name: 'key of 7 characters', input: { api_key: 'k'.repeat(7) }, out: { api_key: 'k'.repeat(7) }, ok: false },
  { name: 'key of 8 characters', input: { api_key: 'k'.repeat(8) }, out: { api_key: 'k'.repeat(8) }, ok: true },
  { name: 'key of 512 characters', input: { api_key: 'k'.repeat(512) }, out: { api_key: 'k'.repeat(512) }, ok: true },
  { name: 'key of 513 characters', input: { api_key: 'k'.repeat(513) }, out: { api_key: 'k'.repeat(513) }, ok: false },
  { name: 'key with a space', input: { api_key: 'sk-ant api03' }, out: { api_key: 'sk-ant api03' }, ok: false },
  { name: 'key with a leading space', input: { api_key: ' sk-ant-api03' }, out: { api_key: ' sk-ant-api03' }, ok: false },
  { name: 'key not ASCII', input: { api_key: 'sk-ключ-abcdef' }, out: { api_key: 'sk-ключ-abcdef' }, ok: false },
  { name: 'base URL with a query', input: { api_key: KEY, base_url: 'https://x.example.com/?a=1' }, out: { api_key: KEY, base_url: 'https://x.example.com/?a=1' }, ok: false },
  { name: 'base URL with a fragment', input: { api_key: KEY, base_url: 'https://x.example.com/#a' }, out: { api_key: KEY, base_url: 'https://x.example.com/#a' }, ok: false },
  { name: 'base URL with a space', input: { api_key: KEY, base_url: 'https://x.example.com/a b' }, out: { api_key: KEY, base_url: 'https://x.example.com/a b' }, ok: false },
  { name: 'base URL not http', input: { api_key: KEY, base_url: 'ftp://x.example.com' }, out: { api_key: KEY, base_url: 'ftp://x.example.com' }, ok: false },
  { name: 'base URL without a host', input: { api_key: KEY, base_url: 'https://' }, out: { api_key: KEY, base_url: 'https://' }, ok: false },
  { name: 'edit: untouched', stored: { api_key: SECRET_MASK, base_url: 'https://custom.example.com' }, out: { api_key: SECRET_MASK, base_url: 'https://custom.example.com' }, ok: true },
  { name: 'edit: the emptied key box keeps the stored key', stored: { api_key: SECRET_MASK }, input: { api_key: '' }, out: { api_key: SECRET_MASK }, ok: true },
  { name: 'edit: a new key replaces it', stored: { api_key: SECRET_MASK }, input: { api_key: 'sk-ant-api03-newkey' }, out: { api_key: 'sk-ant-api03-newkey' }, ok: true },
  { name: 'edit: a short new key', stored: { api_key: SECRET_MASK }, input: { api_key: 'short' }, out: { api_key: 'short' }, ok: false },
  { name: 'edit: a stored base URL the rules refuse', stored: { api_key: SECRET_MASK, base_url: 'ftp://x.example.com' }, out: { api_key: SECRET_MASK, base_url: 'ftp://x.example.com' }, ok: false },
  { name: 'edit: a key the form does not know goes back as it is', stored: { api_key: SECRET_MASK, legacy: 'x' }, input: { base_url: 'https://other.example.com' }, out: { api_key: SECRET_MASK, legacy: 'x', base_url: 'https://other.example.com' }, ok: true }
]

describe('AnthropicKeyForm', () => {
  for (const c of cases) {
    it(c.name, async () => {
      expect(await run(AnthropicKeyForm, c)).toEqual({ value: c.out, ok: c.ok })
    })
  }

  it('labels, help, placeholders and the preset', async () => {
    const { w } = editor(AnthropicKeyForm)
    await flushPromises()
    expect(w.findAll('label').map((l) => l.text())).toEqual(['API key*', 'Base URL'])
    expect(w.find(CONTROL.api_key).attributes('placeholder')).toBe('sk-ant-...')
    expect(w.find(CONTROL.base_url).attributes('placeholder')).toBe(messages.en.baseUrl.placeholder)
    expect(w.text()).toContain(messages.en.apiKey.help)
    expect(w.text()).toContain(messages.en.baseUrl.help)
    expect(w.findAll('button').map((b) => b.text())).toContain('Presets')
  })

  it('guarded base URL: the official value only, unless the caller holds account:settings:custom', async () => {
    const accountType = { plugin_key: 'anthropic', type: 'apikey', guarded_settings: [{ field: 'base_url', allowed: ['https://api.anthropic.com'] }] }
    const guarded = editor(AnthropicKeyForm, { accountType })
    await flushPromises()
    expect(guarded.w.find('select').attributes('disabled')).toBeDefined()
    guarded.w.unmount()
    fakeHost(['account:settings:custom'])
    const free = editor(AnthropicKeyForm, { accountType })
    await flushPromises()
    expect(free.w.find(CONTROL.base_url).exists()).toBe(true)
  })

  it('shows a server error at its field', async () => {
    const { w } = editor(AnthropicKeyForm, { errors: { api_key: 'invalid x-api-key' } })
    await flushPromises()
    expect(w.text()).toContain('invalid x-api-key')
  })
})

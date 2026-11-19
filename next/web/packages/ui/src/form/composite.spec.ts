import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h, nextTick, ref, type Component } from 'vue'
import { ApiError, provideHost, type ApiClient, type HostContext } from '@sub2api/host'
import SSecretInput from '../SSecretInput.vue'
import SUrlPresetInput from '../SUrlPresetInput.vue'
import SResourcePage from '../SResourcePage.vue'
import SSettingRow from '../SSettingRow.vue'
import SSettingsGroup from '../SSettingsGroup.vue'
import SInput from '../SInput.vue'
import SField from '../SField.vue'
import { confirmState, settleConfirm, toastState } from '../feedback'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import { createApiKeyForm } from './apiKeyForm'
import { useCredentialForm, type NativeAccountFormProps } from './credential'
import SCredentialForm from './SCredentialForm.vue'
import { usePluginSettings, type PluginSettingsForm } from './pluginSettings'
import SSettingsPage from './SSettingsPage.vue'
import { SECRET_MASK } from './rules'

type M = Record<string, any>

/** A fake console: the permissions a test grants, everything else unused. */
function fakeHost(granted: string[] = []) {
  provideHost({ permissions: { superuser: () => false, has: (k: string) => granted.includes(k), any: (...k: string[]) => k.some((x) => granted.includes(x)) } } as unknown as HostContext)
}

beforeEach(() => {
  toastState.items.splice(0)
  fakeHost()
})
const mounted: VueWrapper[] = []
afterEach(() => {
  mounted.splice(0).forEach((w) => w.unmount())
  document.body.innerHTML = ''
})

/** Mounts a native account form the way the account editor does: v-model credentials plus the §23.5 props. */
function editor(form: Component, props: Partial<NativeAccountFormProps> & { modelValue?: M } = {}) {
  const credentials = ref<M>(props.modelValue ?? {})
  const errors = ref<Record<string, string> | undefined>(props.errors)
  const inner = ref<{ validate: () => Promise<boolean> }>()
  const w = mount(
    defineComponent({
      setup: () => () =>
        h('form', [
          h(form, {
            ref: inner,
            modelValue: credentials.value,
            'onUpdate:modelValue': (v: M) => (credentials.value = v),
            mode: props.mode ?? 'create',
            account: props.account ?? null,
            accountType: props.accountType ?? { plugin_key: 'anthropic', type: 'apikey' },
            errors: errors.value,
            proxyTicket: async () => 'ticket'
          })
        ])
    }),
    { attachTo: document.body }
  )
  mounted.push(w)
  return { w, credentials, errors, validate: () => inner.value!.validate() }
}

async function type(w: ReturnType<typeof mount>, selector: string, value: string) {
  const el = w.find(selector)
  await el.setValue(value)
  await el.trigger('blur')
  await flushPromises()
}

describe('SSecretInput', () => {
  it('shows a stored secret as an empty box, replaces it on input and keeps it when cleared', async () => {
    const value = ref<string | undefined>(SECRET_MASK)
    const w = mount(() => h(SSecretInput, { modelValue: value.value, 'onUpdate:modelValue': (v: any) => (value.value = v), placeholder: 'sk-…' }))
    const input = w.find('input')
    expect(input.element.value).toBe('')
    expect(input.attributes('placeholder')).toBe(SECRET_MASK)
    expect(input.attributes('autocomplete')).toBe('new-password')
    expect(input.attributes('type')).toBe('password')
    expect(w.text()).toContain('A value is stored. Leave empty to keep it.')
    await input.setValue('new-secret')
    expect(value.value).toBe('new-secret')
    await nextTick()
    expect(w.find('input').attributes('placeholder')).toBe('sk-…')
    await w.find('input').setValue('')
    expect(value.value).toBe(SECRET_MASK)
    // Another account: nothing is stored, clearing sends ''.
    value.value = undefined
    await nextTick()
    await w.find('input').setValue('x')
    await w.find('input').setValue('')
    expect(value.value).toBe('')
  })
})

describe('SUrlPresetInput', () => {
  it('is a URL box with suggestions; empty emits undefined', async () => {
    const out: unknown[] = []
    const w = mount(SUrlPresetInput, { props: { modelValue: '', presets: ['https://api.a.com', 'https://b'], 'onUpdate:modelValue': (v: unknown) => out.push(v) }, attachTo: document.body })
    expect(w.find('input').attributes('placeholder')).toBe('https://api.a.com')
    expect(w.findAll('datalist option').map((o) => o.attributes('value'))).toEqual(['https://api.a.com', 'https://b'])
    await w.find('input').setValue('https://x')
    await w.find('input').setValue('')
    expect(out).toEqual(['https://x', undefined])
    await w.find('button').trigger('click')
    const item = [...document.querySelectorAll('[role="menuitem"]')].find((e) => e.textContent?.includes('https://b')) as HTMLElement
    item.click()
    expect(out.at(-1)).toBe('https://b')
    w.unmount()
  })
  it('offers only the allowed values, keeps a stored one outside them, and locks a single one', async () => {
    const w = mount(SUrlPresetInput, { props: { modelValue: 'https://admin-set', allowed: ['https://official'] } })
    expect(w.findAll('option').map((o) => o.text())).toEqual(['https://admin-set', 'https://official'])
    expect(w.find('select').attributes('disabled')).toBeDefined()
    expect(w.text()).toContain('Set by an administrator')
    const two = mount(SUrlPresetInput, { props: { modelValue: undefined, allowed: ['https://a', 'https://b'], placeholder: 'pick' } })
    expect(two.find('select').attributes('disabled')).toBeUndefined()
    expect(two.findAll('option').map((o) => o.text())).toEqual(['pick', 'https://a', 'https://b'])
    expect(two.text()).not.toContain('Set by an administrator')
  })
})

describe('useCredentialForm / SCredentialForm', () => {
  const Form = defineComponent({
    props: { modelValue: Object, mode: String, account: Object, accountType: Object, errors: Object, proxyTicket: Function },
    emits: ['update:modelValue'],
    setup(props, { emit, expose }) {
      const form = useCredentialForm(props as unknown as NativeAccountFormProps, (_e, v) => emit('update:modelValue', v), {
        defaults: { region: 'us-east-1' },
        rules: { access_key_id: { required: true, pattern: /^[A-Z0-9]{16,128}$/ } }
      })
      expose({ validate: form.validate, creating: form.creating })
      return () =>
        h(SCredentialForm, { form }, () =>
          h(SField, { prop: 'access_key_id', label: 'Key id' }, () => h(SInput, { modelValue: form.get('access_key_id'), 'onUpdate:modelValue': (v: any) => form.set('access_key_id', v) }))
        )
    }
  })

  it('fills defaults when creating only, and lists server errors no field shows', async () => {
    const created = editor(Form, { errors: { region: 'unknown region', access_key_id: 'rejected' } })
    await flushPromises()
    expect(created.credentials.value).toEqual({ region: 'us-east-1' })
    expect(created.w.find('[data-testid="credential-form-errors"]').text()).toBe('unknown region')
    expect(created.w.text()).toContain('rejected')
    // Inside the editor's <form>: no nested form.
    expect(document.querySelectorAll('form form').length).toBe(0)
    const edited = editor(Form, { mode: 'edit', modelValue: { access_key_id: 'AKIAABCDEFGHIJKLMNOP', token_for: 'kept' } })
    await flushPromises()
    expect(edited.credentials.value).toEqual({ access_key_id: 'AKIAABCDEFGHIJKLMNOP', token_for: 'kept' })
    expect(await edited.validate()).toBe(true)
  })
})

// The anthropic account form written with createApiKeyForm (DESIGN §5.10)
// against the schema form it replaces, plugins/anthropic/forms/apikey.schema.json:
//   required: [api_key]; additionalProperties: false; no defaults;
//   api_key: string 8–512, ^[!-~]+$ (ui: secret); base_url: ^(https?://[^\s?#]+)?$ (ui: url-presets,
//   presets [https://api.anthropic.com]).
describe('createApiKeyForm: the anthropic form', () => {
  const Anthropic = createApiKeyForm({
    baseUrl: { presets: ['https://api.anthropic.com'], placeholder: () => 'Leave empty to use https://api.anthropic.com' },
    apiKey: { placeholder: () => 'sk-ant-…', help: () => 'Anthropic Console API key.' }
  })
  const KEY = 'input[type="password"]'
  const URL = 'input[type="url"]'
  /** What the account editor would save (JSON, as sent). */
  const saved = (v: M) => JSON.parse(JSON.stringify(v))

  const cases: Array<{ name: string; key?: string; url?: string; out: M; ok: boolean }> = [
    { name: 'key only', key: 'sk-ant-api03-abcdef', out: { api_key: 'sk-ant-api03-abcdef' }, ok: true },
    { name: 'key and base URL', key: 'sk-ant-api03-abcdef', url: 'https://proxy.example.com/anthropic', out: { api_key: 'sk-ant-api03-abcdef', base_url: 'https://proxy.example.com/anthropic' }, ok: true },
    { name: 'a cleared base URL is not sent', key: 'sk-ant-api03-abcdef', url: '', out: { api_key: 'sk-ant-api03-abcdef' }, ok: true },
    { name: 'no key', out: {}, ok: false },
    { name: 'short key', key: 'sk-1234', out: { api_key: 'sk-1234' }, ok: false },
    { name: 'key with a space', key: 'sk-ant api03', out: { api_key: 'sk-ant api03' }, ok: false },
    { name: 'base URL with a query', key: 'sk-ant-api03-abcdef', url: 'https://x.example.com/?a=1', out: { api_key: 'sk-ant-api03-abcdef', base_url: 'https://x.example.com/?a=1' }, ok: false },
    { name: 'base URL not http', key: 'sk-ant-api03-abcdef', url: 'ftp://x.example.com', out: { api_key: 'sk-ant-api03-abcdef', base_url: 'ftp://x.example.com' }, ok: false },
    { name: 'key of 513 characters', key: 'k'.repeat(513), out: { api_key: 'k'.repeat(513) }, ok: false }
  ]
  for (const c of cases) {
    it(`same keys and verdict as the schema form: ${c.name}`, async () => {
      const { w, credentials, validate } = editor(Anthropic)
      await flushPromises()
      if (c.key !== undefined) await type(w, KEY, c.key)
      if (c.url !== undefined) {
        await type(w, URL, 'https://tmp.example.com')
        await type(w, URL, c.url)
      }
      expect(saved(credentials.value)).toEqual(c.out)
      expect(await validate()).toBe(c.ok)
    })
  }

  it('labels, star, placeholders and the preset menu', async () => {
    const { w } = editor(Anthropic)
    await flushPromises()
    expect(w.findAll('label').map((l) => l.text())).toEqual(['API key*', 'Base URL'])
    expect(w.find(KEY).attributes('placeholder')).toBe('sk-ant-…')
    expect(w.find(URL).attributes('placeholder')).toBe('Leave empty to use https://api.anthropic.com')
    expect(w.text()).toContain('Anthropic Console API key.')
    expect(w.findAll('button').map((b) => b.text())).toContain('Presets')
  })

  it('edit: an untouched stored key and every other key go back as they are', async () => {
    const stored = { api_key: SECRET_MASK, base_url: 'https://custom.example.com', legacy: 'x' }
    const { w, credentials, validate } = editor(Anthropic, { mode: 'edit', modelValue: { ...stored } })
    await flushPromises()
    expect(await validate()).toBe(true)
    expect(credentials.value).toEqual(stored)
    await type(w, URL, 'https://other.example.com')
    expect(credentials.value).toEqual({ ...stored, base_url: 'https://other.example.com' })
  })

  it('shows the server errors at their fields until the field changes', async () => {
    const { w } = editor(Anthropic, { errors: { api_key: 'invalid x-api-key' } })
    await flushPromises()
    expect(w.text()).toContain('invalid x-api-key')
    await type(w, KEY, 'sk-ant-api03-abcdef')
    expect(w.text()).not.toContain('invalid x-api-key')
  })

  it('guarded base URL: the official values only, unless the caller holds account:settings:custom', async () => {
    const accountType = { plugin_key: 'anthropic', type: 'apikey', guarded_settings: [{ field: 'base_url', allowed: ['https://api.anthropic.com'] }] }
    const guarded = editor(Anthropic, { accountType })
    await flushPromises()
    expect(guarded.w.find('select').attributes('disabled')).toBeDefined()
    expect(guarded.w.text()).toContain('Set by an administrator')
    guarded.w.unmount()
    fakeHost(['account:settings:custom'])
    const free = editor(Anthropic, { accountType })
    await flushPromises()
    expect(free.w.find('select').exists()).toBe(false)
    expect(free.w.find(URL).exists()).toBe(true)
  })

  it('extra fields: select with defaults, sections, `when`, date-time in RFC 3339, no base URL', async () => {
    const Bedrock = createApiKeyForm({
      baseUrl: false,
      apiKey: { max: 2048 },
      defaults: { region: 'us-east-1', inference_profile_prefix: 'global' },
      extra: [
        { key: 'region', kind: 'text', label: () => 'Region', section: () => 'Where', rules: { pattern: /^([a-z]{2}(-[a-z]+)+-\d)?$/ } },
        { key: 'inference_profile_prefix', kind: 'select', label: () => 'Prefix', section: () => 'Where', options: ['none', 'us', 'global'].map((v) => ({ value: v, label: () => v.toUpperCase() })), rules: { enum: ['none', 'us', 'global'] } },
        { key: 'expires', kind: 'date-time', label: () => 'Expires', when: (m) => m.inference_profile_prefix === 'us', rules: { required: true, type: 'date-time' } }
      ]
    })
    const { w, credentials, validate } = editor(Bedrock)
    await flushPromises()
    expect(credentials.value).toEqual({ region: 'us-east-1', inference_profile_prefix: 'global' })
    expect(w.find('input[type="url"]').exists()).toBe(false)
    expect(w.findAll('h4').map((x) => x.text())).toEqual(['Where'])
    expect(w.find('input[type="datetime-local"]').exists()).toBe(false)
    await type(w, KEY, 'k'.repeat(1000))
    expect(await validate()).toBe(true)
    await w.find('select').setValue(String(1))
    await flushPromises()
    expect(credentials.value.inference_profile_prefix).toBe('us')
    expect(await validate()).toBe(false)
    await w.find('input[type="datetime-local"]').setValue('2026-11-19T10:00')
    await w.find('input[type="datetime-local"]').trigger('change')
    await flushPromises()
    expect(credentials.value.expires).toMatch(/^2026-11-19T\d{2}:00:00(\.000)?(Z|[+-]\d{2}:\d{2})$/)
    expect(await validate()).toBe(true)
    await type(w, 'input[type="text"]', 'nowhere')
    expect(await validate()).toBe(false)
    expect(w.text()).toContain('Invalid format')
  })
})

/** A fake host.api for GET / PUT /plugins/demo/settings. */
function settingsApi(view: M, put: (body: M) => Promise<unknown> = async () => null) {
  const calls: M[] = []
  const api = {
    get: async (path: string) => {
      expect(path).toBe('/plugins/demo/settings')
      return JSON.parse(JSON.stringify(view))
    },
    put: async (path: string, body: M) => {
      expect(path).toBe('/plugins/demo/settings')
      calls.push(body)
      return put(body)
    }
  } as unknown as ApiClient
  return { api, calls }
}

function settingsPage(api: ApiClient, defaults: M = { port: 587, from_name: 'Sub2API' }) {
  let settings!: PluginSettingsForm<M>
  const w = mount(
    defineComponent({
      setup() {
        settings = usePluginSettings<M>({ pluginKey: 'demo', api, defaults, rules: { host: { max: 253 }, port: { required: true, type: 'integer', min: 1, max: 65535 } } })
        return () =>
          h(SSettingsPage, { settings, title: 'Mailer' }, () =>
            h(SSettingsGroup, { title: 'SMTP' }, () => [
              h(SSettingRow, { prop: 'host', label: 'Host' }, () => h(SInput, { modelValue: settings.get('host'), 'onUpdate:modelValue': (v: any) => settings.set('host', v) })),
              h(SSettingRow, { prop: 'port', label: 'Port' }, () => h(SInput, { type: 'number', modelValue: settings.get('port'), 'onUpdate:modelValue': (v: any) => settings.set('port', v) })),
              h(SSettingRow, { prop: 'password', label: 'Password' }, () => h(SSecretInput, { modelValue: settings.get('password'), 'onUpdate:modelValue': (v: any) => settings.set('password', v) }))
            ])
          )
      }
    }),
    { attachTo: document.body }
  )
  mounted.push(w)
  return { w, settings: () => settings }
}

describe('usePluginSettings / SSettingsPage', () => {
  it('loads, fills defaults into the baseline, tracks unsaved changes, saves the whole value', async () => {
    fakeHost(['plugin:manage'])
    const { api, calls } = settingsApi({ values: { host: 'smtp.example.com', password: SECRET_MASK, extra: { keep: 1 } } })
    const { w, settings } = settingsPage(api)
    await flushPromises()
    expect(settings().get('port')).toBe(587)
    expect(settings().dirty.value).toBe(false)
    expect(w.find('[data-testid="settings-save"]').attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="plugin-settings-dirty"]').exists()).toBe(false)
    await w.find('input').setValue('mail.example.com')
    await flushPromises()
    expect(settings().dirty.value).toBe(true)
    expect(w.find('[data-testid="plugin-settings-dirty"]').text()).toBe('You have unsaved changes')
    await w.find('[data-testid="settings-save"]').trigger('click')
    await flushPromises()
    expect(calls).toEqual([{ values: { host: 'mail.example.com', password: SECRET_MASK, extra: { keep: 1 }, port: 587, from_name: 'Sub2API' } }])
    expect(toastState.items.map((t) => t.message)).toEqual(['Saved'])
    expect(settings().dirty.value).toBe(false)
  })

  it('reset goes back to the loaded value', async () => {
    fakeHost(['plugin:manage'])
    const { api } = settingsApi({ values: { host: 'a' } })
    const { w, settings } = settingsPage(api)
    await flushPromises()
    await w.find('input').setValue('b')
    await flushPromises()
    await w.find('[data-testid="settings-reset"]').trigger('click')
    await flushPromises()
    expect(settings().get('host')).toBe('a')
    expect(settings().dirty.value).toBe(false)
  })

  it('local rules block the save; server field errors show at their rows, reenter in its own words, root ones as a toast', async () => {
    const refusal = new ApiError(400, {
      code: 'invalid_argument',
      message: 'invalid settings',
      details: { fields: [{ field: 'values/password', code: 'reenter', message: 're-enter this secret to change settings' }, { field: 'values/host', code: 'invalid', message: 'unknown host' }, { field: 'values', code: 'invalid', message: 'whole object bad' }] }
    })
    const { api, calls } = settingsApi({ values: { host: 'a', port: 25, password: SECRET_MASK }, permission: 'plugin.demo:settings' }, () => Promise.reject(refusal))
    fakeHost(['plugin.demo:settings'])
    const { w, settings } = settingsPage(api)
    await flushPromises()
    expect(settings().canEdit.value).toBe(true)
    settings().set('port', 70000)
    expect(await settings().save()).toBe(false)
    expect(calls).toEqual([])
    expect(w.text()).toContain('Must be ≤ 65535')
    settings().set('port', 25)
    settings().set('host', 'b')
    await flushPromises()
    expect(await settings().save()).toBe(false)
    await flushPromises()
    expect(calls.length).toBe(1)
    expect(w.text()).toContain('Re-enter this secret to change other settings')
    expect(w.text()).toContain('unknown host')
    expect(toastState.items.map((t) => t.message)).toEqual(['whole object bad'])
    await w.find('input').setValue('c')
    await flushPromises()
    expect(w.text()).not.toContain('unknown host')
  })

  it('without edit rights: controls disabled, no save bar', async () => {
    const { api } = settingsApi({ values: { host: 'a' }, permission: 'plugin.demo:settings' })
    const { w, settings } = settingsPage(api)
    await flushPromises()
    expect(settings().canEdit.value).toBe(false)
    expect(w.find('fieldset').attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="settings-save-bar"]').exists()).toBe(false)
  })

  it('a failed load offers a retry', async () => {
    fakeHost(['plugin:manage'])
    let fail = true
    const api = { get: async () => (fail ? Promise.reject(new Error('boom')) : { values: {} }), put: async () => null } as unknown as ApiClient
    const { w } = settingsPage(api)
    await flushPromises()
    expect(w.text()).toContain('boom')
    fail = false
    await w.findAll('button').find((b) => b.text() === 'Retry')!.trigger('click')
    await flushPromises()
    expect(w.text()).not.toContain('boom')
    expect(w.find('fieldset').exists()).toBe(true)
  })

  it('asks before leaving the page with unsaved changes', async () => {
    fakeHost(['plugin:manage'])
    const { api } = settingsApi({ values: { host: 'a' } })
    const { w } = settingsPage(api)
    await flushPromises()
    const ev = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(false)
    await w.find('input').setValue('b')
    await flushPromises()
    const dirty = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(dirty)
    expect(dirty.defaultPrevented).toBe(true)
  })
})

describe('SSettingsPage route leave', () => {
  it('asks before leaving the route with unsaved changes', async () => {
    fakeHost(['plugin:manage'])
    const { api } = settingsApi({ values: { host: 'a' } })
    let settings!: PluginSettingsForm<M>
    const Page = defineComponent({
      setup() {
        settings = usePluginSettings<M>({ pluginKey: 'demo', api, rules: {} })
        return () => h(SSettingsPage, { settings })
      }
    })
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: Page }, { path: '/other', component: { render: () => h('p', 'other') } }] })
    await router.push('/')
    const w = mount(RouterView, { global: { plugins: [router] } })
    mounted.push(w)
    await flushPromises()
    settings.set('host', 'b')
    const leaving = router.push('/other')
    await flushPromises()
    expect(confirmState.current?.title).toBe('Discard unsaved changes?')
    settleConfirm(false)
    await leaving
    expect(router.currentRoute.value.path).toBe('/')
    const again = router.push('/other')
    await flushPromises()
    settleConfirm(true)
    await again
    expect(router.currentRoute.value.path).toBe('/other')
  })
})

describe('SResourcePage', () => {
  function fakeList(total = 45) {
    const queries: M[] = []
    const posts: M[] = []
    const client = {
      list: async (path: string, q: M) => {
        expect(path).toBe('/assets')
        queries.push(q)
        return { items: [{ id: q.page * 100, name: `asset ${q.q ?? ''}` }], page: { page: q.page, page_size: q.page_size, total } }
      },
      post: async (_p: string, body: M) => posts.push(body)
    } as unknown as ApiClient
    return { client, queries, posts }
  }

  it('lists, searches by the named parameter, pages, and renders cell slots', async () => {
    const { client, queries } = fakeList()
    const w = mount(SResourcePage, {
      props: { client, path: '/assets', columns: [{ key: 'id', label: 'ID' }, { key: 'name', label: 'Name' }], search: 'q', rowKey: 'id' },
      slots: { 'cell-name': ({ row }: any) => h('b', row.name) },
      attachTo: document.body
    })
    await flushPromises()
    expect(queries).toEqual([{ page: 1, page_size: 20, q: '' }])
    expect(w.find('b').text()).toBe('asset')
    await w.find('input[type="search"]').setValue('cat')
    await new Promise((r) => setTimeout(r, 300))
    await flushPromises()
    expect(queries.at(-1)).toEqual({ page: 1, page_size: 20, q: 'cat' })
    await w.findAll('button').find((b) => b.text() === '2')!.trigger('click')
    await flushPromises()
    expect(queries.at(-1)).toMatchObject({ page: 2 })
    expect(w.find('[data-testid="resource-create"]').exists()).toBe(false)
    w.unmount()
  })

  it('creates through the dialog, then reloads page 1 and emits created', async () => {
    const { client, queries, posts } = fakeList()
    const name = ref('')
    let accept = false
    const submit = async () => {
      if (!accept) return false
      await client.post('/assets', { name: name.value })
    }
    const created: unknown[] = []
    const w = mount(SResourcePage, {
      props: { client, path: '/assets', columns: [{ key: 'id', label: 'ID' }], canCreate: true, createLabel: 'New asset', submit, onCreated: () => created.push(1) },
      slots: { create: () => h(SInput, { modelValue: name.value, 'onUpdate:modelValue': (v: any) => (name.value = v) }) },
      attachTo: document.body
    })
    await flushPromises()
    await w.findAll('button').find((b) => b.text() === '2')!.trigger('click')
    await flushPromises()
    await w.find('[data-testid="resource-create"]').trigger('click')
    await flushPromises()
    const save = () => [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Save') as HTMLButtonElement
    save().click()
    await flushPromises()
    expect(created).toEqual([])
    expect(document.body.textContent).toContain('New asset')
    accept = true
    name.value = 'logo'
    save().click()
    await flushPromises()
    expect(posts).toEqual([{ name: 'logo' }])
    expect(created).toEqual([1])
    expect(queries.at(-1)).toMatchObject({ page: 1 })
    w.unmount()
  })
})

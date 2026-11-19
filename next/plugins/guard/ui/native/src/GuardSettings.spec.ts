import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { provideHost, type HostContext, type PluginHost } from '@sub2api/host'
import { SSettingsPage, type PluginSettingsForm } from '@sub2api/ui'
import GuardSettings from './GuardSettings.vue'
import messages from './messages'
import { setHost } from './host'

type M = Record<string, any>
const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v))

/** The value the form fills into empty settings (the plugin's own defaults). */
const DEFAULTS = { webhook_url: '', record_snippets: false, alert_cooldown_sec: 60 }

/** A fake core for GET / PUT /plugins/guard/settings (no secrets: ui.settings.secrets is []). */
function fakeCore(values: M) {
  let stored: M = clone(values)
  const puts: M[] = []
  const api = {
    get: async (path: string) => {
      expect(path).toBe('/plugins/guard/settings')
      return { values: clone(stored) }
    },
    put: async (path: string, body: { values: M }) => {
      expect(path).toBe('/plugins/guard/settings')
      puts.push(clone(body.values))
      stored = clone(body.values)
      return null
    }
  }
  return { api, puts }
}

let wrapper: { unmount(): void } | null = null
async function mountSettings(values: M = {}, granted = ['plugin:manage']) {
  const core = fakeCore(values)
  provideHost({
    permissions: { superuser: () => false, has: (k: string) => granted.includes(k), any: (...k: string[]) => k.some((x) => granted.includes(x)) },
    i18n: { t: (k: string) => k }
  } as unknown as HostContext)
  setHost({ t: (k: string) => k, plugin: { key: 'guard' }, api: core.api } as unknown as PluginHost)
  const w = mount(GuardSettings, { attachTo: document.body })
  wrapper = w
  await flushPromises()
  const settings = w.findComponent(SSettingsPage).props('settings') as PluginSettingsForm<M>
  return { w, core, settings }
}
const field = (w: Awaited<ReturnType<typeof mountSettings>>['w'], id: string) => w.get(`[data-testid="guard-${id}"]`)

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  setHost(null)
  document.body.innerHTML = ''
})

describe('GuardSettings', () => {
  it('fills the defaults into unset settings and saves the whole value', async () => {
    const { w, core, settings } = await mountSettings({})
    expect(settings.value()).toEqual(DEFAULTS)
    expect(settings.dirty.value).toBe(false)
    await field(w, 'webhook-url').get('input').setValue('https://hooks.example.com/guard')
    await field(w, 'snippets').setValue(true)
    await field(w, 'cooldown').setValue('0')
    expect(await settings.save()).toBe(true)
    expect(core.puts).toEqual([{ webhook_url: 'https://hooks.example.com/guard', record_snippets: true, alert_cooldown_sec: 0 }])
  })

  it('keeps stored keys and fills only the absent ones (older settings)', async () => {
    const { settings } = await mountSettings({ webhook_url: 'https://hooks.example.com/x' })
    expect(settings.value()).toEqual({ ...DEFAULTS, webhook_url: 'https://hooks.example.com/x' })
  })

  it('labels, help, unit and placeholder', async () => {
    const { w } = await mountSettings({})
    for (const key of ['settings.webhookUrl.label', 'settings.webhookUrl.help', 'settings.cooldown.label', 'settings.cooldown.help', 'settings.cooldown.unit', 'settings.snippets.label', 'settings.snippets.help'])
      expect(w.text()).toContain(key)
    expect(field(w, 'webhook-url').get('input').attributes('placeholder')).toBe('https://hooks.example.com/guard')
  })

  it('a cleared webhook URL or cooldown sends no key (the plugin then sends no alert / uses 60 s)', async () => {
    const { w, core, settings } = await mountSettings({ webhook_url: 'https://hooks.example.com/x', alert_cooldown_sec: 30 })
    await field(w, 'webhook-url').get('input').setValue('')
    await field(w, 'cooldown').setValue('')
    expect(await settings.save()).toBe(true)
    expect(core.puts[0]).toEqual({ record_snippets: false })
  })

  it('without edit rights every control is disabled and there is no save bar', async () => {
    const { w } = await mountSettings({}, [])
    expect(w.find('fieldset').attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="settings-save-bar"]').exists()).toBe(false)
  })
})

// One case per constraint of the JSON Schema form it replaces (written out:
// the schema file is gone): the value the form saves and whether it lets it
// be saved. Checked against that form on 45 inputs before it was deleted:
// the same values and verdicts.
describe('GuardSettings rules', () => {
  const cases: Array<{ name: string; set: M; ok: boolean }> = [
    { name: 'defaults', set: {}, ok: true },
    { name: 'a webhook with a token', set: { webhook_url: 'https://hooks.example.com/guard?token=x', record_snippets: true, alert_cooldown_sec: 0 }, ok: true },
    { name: 'an http webhook', set: { webhook_url: 'http://10.0.0.5:8080/hook' }, ok: true },
    { name: 'a webhook that is not http(s)', set: { webhook_url: 'ftp://hooks.example.com' }, ok: false },
    { name: 'a webhook with a space', set: { webhook_url: 'https://hooks.example.com/a b' }, ok: false },
    { name: 'a scheme without a host', set: { webhook_url: 'https://' }, ok: false },
    { name: 'cooldown below 0', set: { alert_cooldown_sec: -1 }, ok: false },
    { name: 'cooldown above 300', set: { alert_cooldown_sec: 301 }, ok: false },
    { name: 'cooldown 300', set: { alert_cooldown_sec: 300 }, ok: true },
    { name: 'fractional cooldown', set: { alert_cooldown_sec: 1.5 }, ok: false },
    // Kept as it is; the plugin's ValidateSettings refuses it (code unknown).
    { name: 'a key the form does not know', set: { other: 'x' }, ok: true }
  ]
  for (const c of cases) {
    it(c.name, async () => {
      const { settings } = await mountSettings({})
      for (const [k, v] of Object.entries(c.set)) settings.set(k, v)
      expect(settings.value()).toEqual({ ...DEFAULTS, ...c.set })
      expect(await settings.validate()).toBe(c.ok)
    })
  }
})

describe('messages', () => {
  const keys = (o: unknown, prefix = ''): string[] =>
    o && typeof o === 'object' ? Object.entries(o).flatMap(([k, v]) => keys(v, prefix ? `${prefix}.${k}` : k)) : [prefix]
  it('zh and en have the same keys; the settings texts hold no vue-i18n special characters', () => {
    expect(keys(messages.zh).sort()).toEqual(keys(messages.en).sort())
    const leaves = (o: unknown): string[] => (o && typeof o === 'object' ? Object.values(o).flatMap(leaves) : [String(o)])
    for (const lang of [messages.zh.settings, messages.en.settings]) expect(leaves(lang).filter((s) => /[@|{}]/.test(s))).toEqual([])
  })
})

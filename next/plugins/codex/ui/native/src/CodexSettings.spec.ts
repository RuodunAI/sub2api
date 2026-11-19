import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { provideHost, type HostContext, type PluginHost } from '@sub2api/host'
import { SSettingsPage, type PluginSettingsForm } from '@sub2api/ui'
import CodexSettings from './CodexSettings.vue'
import { setHost } from './host'

type M = Record<string, any>
const clone = <T>(v: T): T => JSON.parse(JSON.stringify(v))

/** A fake core for GET / PUT /plugins/codex/settings (no secrets: ui.settings.secrets is empty). */
function fakeCore(values: M) {
  let stored: M = clone(values)
  const puts: M[] = []
  const api = {
    get: async (path: string) => {
      expect(path).toBe('/plugins/codex/settings')
      return { values: clone(stored) }
    },
    put: async (path: string, body: { values: M }) => {
      expect(path).toBe('/plugins/codex/settings')
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
  setHost({ t: (k: string) => k, plugin: { key: 'codex' }, api: core.api } as unknown as PluginHost)
  const w = mount(CodexSettings, { attachTo: document.body })
  wrapper = w
  await flushPromises()
  const settings = w.findComponent(SSettingsPage).props('settings') as PluginSettingsForm<M>
  return { w, core, settings }
}
const input = (w: Awaited<ReturnType<typeof mountSettings>>['w'], id: string) => w.get(`[data-testid="codex-settings-${id}"]`)

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  setHost(null)
  document.body.innerHTML = ''
})

describe('CodexSettings', () => {
  it('fills both headers empty (the built-in ones) and saves the whole value', async () => {
    const { w, core, settings } = await mountSettings({})
    expect(settings.value()).toEqual({ user_agent: '', originator: '' })
    expect(settings.dirty.value).toBe(false)
    await input(w, 'user-agent').setValue('my-agent/1.0')
    expect(await settings.save()).toBe(true)
    expect(core.puts).toEqual([{ user_agent: 'my-agent/1.0', originator: '' }])
  })

  it('labels, help and placeholders', async () => {
    const { w } = await mountSettings({ originator: 'ops' })
    for (const key of ['settings.userAgent', 'settings.userAgentHint', 'settings.originator', 'settings.originatorHint']) expect(w.text()).toContain(key)
    expect(input(w, 'user-agent').attributes('placeholder')).toBe('sup2api-codex/<version>')
    expect(input(w, 'originator').attributes('placeholder')).toBe('sup2api')
    expect((input(w, 'originator').element as HTMLInputElement).value).toBe('ops')
  })

  it('without edit rights every control is disabled and there is no save bar', async () => {
    const { w } = await mountSettings({}, [])
    expect(w.find('fieldset').attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="settings-save-bar"]').exists()).toBe(false)
  })
})

// One case per constraint of the JSON Schema form it replaces (written out:
// the schema file is gone): the value the form saves and whether it lets it
// be saved. Checked against that form before it was deleted (see the
// commit): the same values and verdicts.
describe('CodexSettings rules', () => {
  const cases: Array<{ name: string; set: M; ok: boolean }> = [
    { name: 'defaults', set: {}, ok: true },
    { name: 'both set', set: { user_agent: 'sup2api-codex/0.2.0', originator: 'codex_cli_rs' }, ok: true },
    { name: 'a tab is allowed', set: { user_agent: 'a\tb' }, ok: true },
    { name: 'a line break in the User-Agent', set: { user_agent: 'a\nb' }, ok: false },
    { name: 'a NUL in the originator', set: { originator: 'a\u0000' }, ok: false },
    { name: 'DEL in the originator', set: { originator: 'a\u007f' }, ok: false },
    { name: 'a unit separator in the User-Agent', set: { user_agent: '\u001f' }, ok: false },
    // Kept as it is; the plugin's ValidateSettings refuses it (code unknown).
    { name: 'a key the form does not know', set: { other: 'x' }, ok: true }
  ]
  for (const c of cases) {
    it(c.name, async () => {
      const { settings } = await mountSettings({})
      for (const [k, v] of Object.entries(c.set)) settings.set(k, v)
      expect(settings.value()).toEqual({ user_agent: '', originator: '', ...c.set })
      expect(await settings.validate()).toBe(c.ok)
    })
  }
})

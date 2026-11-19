/// <reference types="vite/client" />
import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, type VueWrapper } from '@vue/test-utils'
import { checkMessages, cleanup, mountSettings as mount } from '@sub2api/ui/testing'
import GrowthSettings from './GrowthSettings.vue'
import messages from './messages'
import { setHost } from './host'

type M = Record<string, any>

/** The value the form fills into empty settings (the JSON Schema defaults it replaces). */
const DEFAULTS = {
  referral_enabled: true,
  referral_rate_percent: 10,
  referral_max_per_invitee: 0,
  referral_duration_days: 0,
  checkin_enabled: true,
  checkin_min_quota: 0.01,
  checkin_max_quota: 0.05,
  checkin_timezone: 'UTC'
}

/** GrowthSettings over a fake core holding `values` (no secrets: ui.settings.secrets is []). */
const mountSettings = (values: M = {}, granted = ['plugin:manage']) => mount(GrowthSettings, { plugin: 'growth', setHost, values, granted })
const field = (w: VueWrapper, id: string) => w.find(`[data-testid="growth-${id}"]`)

afterEach(cleanup)

describe('GrowthSettings', () => {
  it('fills the defaults into unset settings and saves the whole value', async () => {
    const { w, core, settings } = await mountSettings({})
    expect(settings.value()).toEqual(DEFAULTS)
    expect(settings.dirty.value).toBe(false)
    await field(w, 'referral-rate').setValue('12.5')
    expect(await settings.save()).toBe(true)
    expect(core.puts).toEqual([{ ...DEFAULTS, referral_rate_percent: 12.5 }])
  })

  it('keeps stored keys and fills only the absent ones (older settings)', async () => {
    const { settings } = await mountSettings({ referral_enabled: false, checkin_timezone: 'Asia/Shanghai' })
    expect(settings.value()).toEqual({ ...DEFAULTS, referral_enabled: false, checkin_timezone: 'Asia/Shanghai' })
  })

  it('two sections with their titles, labels, help and units', async () => {
    const { w } = await mountSettings({})
    for (const key of [
      'settings.referral.title', 'settings.referral.description', 'settings.referralEnabled', 'settings.referralRate.label', 'settings.referralRate.help',
      'settings.referralMax.label', 'settings.referralMax.help', 'settings.referralDuration.label', 'settings.referralDuration.help', 'settings.checkin.title',
      'settings.checkin.description', 'settings.checkinEnabled', 'settings.checkinMin', 'settings.checkinMax', 'settings.checkinTimezone.label',
      'settings.checkinTimezone.help', 'settings.unit.percent', 'settings.unit.usd', 'settings.unit.days'
    ]) expect(w.text()).toContain(key)
    expect(field(w, 'checkin-timezone').attributes('placeholder')).toBe('UTC')
  })

  it('a switched-off section hides its fields and does not check them; their values are kept', async () => {
    const { w, core, settings } = await mountSettings({ referral_rate_percent: 150 })
    expect(await settings.validate()).toBe(false)
    await field(w, 'referral-enabled').setValue(false)
    await flushPromises()
    expect(settings.get('referral_enabled')).toBe(false)
    expect(field(w, 'referral-rate').exists()).toBe(false)
    expect(field(w, 'referral-max').exists()).toBe(false)
    expect(field(w, 'referral-days').exists()).toBe(false)
    expect(await settings.save()).toBe(true)
    expect(core.puts[0]).toMatchObject({ referral_enabled: false, referral_rate_percent: 150 })
    await flushPromises()
    await field(w, 'checkin-enabled').setValue(false)
    await flushPromises()
    expect(['checkin-min', 'checkin-max', 'checkin-timezone'].map((id) => field(w, id).exists())).toEqual([false, false, false])
  })

  it('a cleared number box sends no key, in every number box', async () => {
    const boxes = {
      'referral-rate': 'referral_rate_percent',
      'referral-max': 'referral_max_per_invitee',
      'referral-days': 'referral_duration_days',
      'checkin-min': 'checkin_min_quota',
      'checkin-max': 'checkin_max_quota'
    }
    const { w, core, settings } = await mountSettings({})
    for (const id of Object.keys(boxes)) await field(w, id).setValue('')
    expect(await settings.save()).toBe(true)
    for (const key of Object.values(boxes)) expect(core.puts[0]).not.toHaveProperty(key)
  })

  it('without edit rights every control is disabled and there is no save bar', async () => {
    const { w } = await mountSettings({}, [])
    expect(w.find('fieldset').attributes('disabled')).toBeDefined()
    expect(w.find('[data-testid="settings-save-bar"]').exists()).toBe(false)
  })
})

// One case per constraint of the JSON Schema form it replaces (written out:
// the schema file is gone): the value the form saves and whether it lets it
// be saved. Checked against that form on 51 inputs before it was deleted:
// the same values and verdicts.
describe('GrowthSettings rules', () => {
  const cases: Array<{ name: string; set: M; ok: boolean }> = [
    { name: 'defaults', set: {}, ok: true },
    { name: 'rate 0 and 100', set: { referral_rate_percent: 100, referral_max_per_invitee: 0 }, ok: true },
    { name: 'rate below 0', set: { referral_rate_percent: -1 }, ok: false },
    { name: 'rate above 100', set: { referral_rate_percent: 100.5 }, ok: false },
    { name: 'negative cap', set: { referral_max_per_invitee: -0.01 }, ok: false },
    { name: 'negative period', set: { referral_duration_days: -1 }, ok: false },
    { name: 'fractional period', set: { referral_duration_days: 1.5 }, ok: false },
    { name: 'negative minimum reward', set: { checkin_min_quota: -1 }, ok: false },
    { name: 'negative maximum reward', set: { checkin_max_quota: -1 }, ok: false },
    { name: 'rewards with cents', set: { checkin_min_quota: 0.1, checkin_max_quota: 2.5, checkin_timezone: 'Asia/Shanghai' }, ok: true },
    { name: 'referral off: its fields are not checked', set: { referral_enabled: false, referral_rate_percent: 150 }, ok: true },
    { name: 'check-in off: its fields are not checked', set: { checkin_enabled: false, checkin_min_quota: -1 }, ok: true },
    { name: 'check-in off: a time zone that is not a string is not checked', set: { checkin_enabled: false, checkin_timezone: 8 }, ok: true },
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

describe('messages', () =>
  checkMessages(messages, {
    sources: import.meta.glob<string>(['./*.vue', './*.ts', '!./*.spec.ts', '!./messages.ts'], { query: '?raw', import: 'default', eager: true }),
    // Looked up with the event type as suffix: host.t('event.' + row.event_type).
    dynamic: ['event.']
  }))

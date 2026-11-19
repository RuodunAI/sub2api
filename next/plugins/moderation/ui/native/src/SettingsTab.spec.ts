import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { ApiError, type PluginHost } from '@sub2api/host'
import { SInput, SSecretInput, SSegmented, SSettingRow, SSwitch } from '@sub2api/ui'
import SettingsTab from './SettingsTab.vue'
import DurationInput from './DurationInput.vue'
import JevQuestionsEditor from './JevQuestionsEditor.vue'
import { setHost } from './host'
import { resetSessionCaches } from './session'

// The settings pages as a user edits them: what each save PUTs and what it
// refuses. The values below are those of the settings form removed in
// 0.12.2 (forms/settings.schema.json): its defaults, values it accepted and
// values it refused, written out so that the pages keep to them. Compared
// with the form-rendered pages before they were removed (222 cases; the 10
// that differed - an unknown option value, an invalid Jev table and a stored
// value out of range while another key changed - the old page left to the
// core, which refused them; these pages refuse them first).

type Call = (path: string, body?: unknown) => Promise<unknown>
const coreGet = vi.fn<Call>()
const corePut = vi.fn<Call>()
const pluginGet = vi.fn<Call>()
const toast = vi.fn()
let stored: Record<string, unknown> = {}
vi.setConfig({ testTimeout: 120000 })
const MASK = '******'
const ROW = { category: 'violence', label: '', question: 'q?', answer_yes: '', answer_no: '', threshold: 0.5 }
const BUILTIN = { preamble: 'builtin', items: [ROW] }

beforeEach(() => {
  stored = {}
  for (const f of [coreGet, corePut, pluginGet, toast]) f.mockReset()
  coreGet.mockImplementation(async () => ({ values: structuredClone(stored), secret_fields: ['api_key', 'notify_webhook_secret', 'notify_webhook_url'] }))
  corePut.mockImplementation(async () => ({ values: stored }))
  pluginGet.mockImplementation(async (path) => (path === '/jev/default-questions' ? { builtin: BUILTIN, builtin_version: 'v3' } : path === '/status' ? { lists_imported: true } : []))
  const t = (key: string) => key
  setHost({
    t,
    i18n: { t, text: (v: { zh?: string } | string) => (typeof v === 'string' ? v : (v?.zh ?? '')), formatNumber: String, locale: { value: 'zh' } },
    plugin: { key: 'moderation' },
    pluginApi: { get: pluginGet, post: vi.fn(), put: vi.fn(), del: vi.fn() },
    api: { get: coreGet, put: corePut },
    permissions: { superuser: () => true, has: () => true, hasAction: () => true },
    can: () => true,
    toast,
    confirm: vi.fn(async () => true)
  } as unknown as PluginHost)
})
afterEach(() => {
  resetSessionCaches()
  setHost(null)
})
enableAutoUnmount(afterEach)

/** The defaults of the settings form. */
const DEFAULTS = {
  mode: 'off', fail_policy: 'open', block_status: 403, block_message: 'Your request was blocked by content moderation. Please revise your input and try again.',
  auto_watch: true, watch_days: 30, escalate_violations: 0, escalate_window_seconds: 3600, ban_threshold: 0, ban_window_seconds: 86400, ban_duration_hours: 24,
  error_rate_alert_percent: 5, notify_webhook_url: '', notify_webhook_secret: '', record_pass: false, record_overturned: true, record_errors: true, store_text: true,
  record_pass_input_message: true, hit_retention_days: 180, overturned_retention_days: 30, error_retention_days: 7, pass_retention_days: 3, stats_retention_days: 90,
  max_concurrency: 16, queue_size: 1000, ttft_timeout_ms: 10000, verdict_cache_days: 7, hit_replay: 'new_content', cache_entries: 100000, input_max_chars: 12000,
  input_max_chunks: 8, input_overflow: 'sample', input_max_images: 8, max_inflight_mb: 128, error_text_max_chars: 2000, min_chars: 2, jev_default_set: null, api_key: ''
}
/** Settings as GET returns them with every optional key set, but the 0.1 API key (secrets masked). */
const STORED = { ...DEFAULTS, notify_webhook_url: MASK, notify_webhook_secret: MASK, jev_default_set: { preamble: 'p', items: [ROW] } } as Record<string, unknown>
delete STORED.api_key

const PAGES = {
  policy: ['mode', 'fail_policy'],
  cache: ['verdict_cache_days', 'hit_replay', 'cache_entries', 'min_chars', 'input_max_chars', 'input_max_chunks', 'input_overflow', 'input_max_images'],
  lists: ['auto_watch', 'watch_days', 'escalate_violations', 'escalate_window_seconds', 'ban_threshold', 'ban_window_seconds', 'ban_duration_hours'],
  jev: ['jev_default_set'],
  response: ['block_status', 'block_message'],
  records: ['record_pass', 'record_overturned', 'record_errors', 'store_text', 'record_pass_input_message', 'error_text_max_chars', 'hit_retention_days', 'overturned_retention_days', 'error_retention_days', 'pass_retention_days', 'stats_retention_days'],
  advanced: ['max_concurrency', 'queue_size', 'max_inflight_mb', 'ttft_timeout_ms', 'error_rate_alert_percent', 'notify_webhook_url', 'notify_webhook_secret']
} as const
type Page = keyof typeof PAGES

/** A value the form accepted for each key, other than its default. */
const LEGAL: Record<string, unknown> = {
  mode: 'enforce', fail_policy: 'close_pre', block_status: 451, block_message: 'blocked', auto_watch: false, watch_days: 60, escalate_violations: 5,
  escalate_window_seconds: 7200, ban_threshold: 3, ban_window_seconds: 600, ban_duration_hours: 0, error_rate_alert_percent: 50,
  notify_webhook_url: 'https://hooks.example/new', notify_webhook_secret: 'new-secret', record_pass: true, record_overturned: false, record_errors: false,
  store_text: false, record_pass_input_message: false, hit_retention_days: 365, overturned_retention_days: 60, error_retention_days: 14,
  pass_retention_days: 1, stats_retention_days: 30, max_concurrency: 32, queue_size: 5000, ttft_timeout_ms: 20000, verdict_cache_days: 0,
  hit_replay: 'whole', cache_entries: 5000, input_max_chars: 1000, input_max_chunks: 4, input_overflow: 'block', input_max_images: 0,
  max_inflight_mb: 256, error_text_max_chars: 0, min_chars: 10
}
/** Values the form refused (a switch offers no such value). */
const ILLEGAL: Record<string, unknown[]> = {
  mode: ['bogus'],
  fail_policy: ['bogus'],
  block_status: [399, 600, 400.5],
  block_message: ['x'.repeat(501)],
  watch_days: [0, 366, 1.5],
  escalate_violations: [-1, 1001, 0.5],
  escalate_window_seconds: [0, 31536001, 1.5],
  ban_threshold: [-1, 1001, 0.5],
  ban_window_seconds: [0, 31536001, 1.5],
  ban_duration_hours: [-1, 87601, 0.5],
  error_rate_alert_percent: [-1, 101, 0.5],
  notify_webhook_url: ['ftp://x', 'https://a b', 'https://' + 'x'.repeat(993)],
  notify_webhook_secret: ['x'.repeat(201)],
  hit_retention_days: [0, 3651, 1.5],
  overturned_retention_days: [0, 3651, 1.5],
  error_retention_days: [0, 3651, 1.5],
  pass_retention_days: [0, 4, 1.5],
  stats_retention_days: [6, 3651, 7.5],
  max_concurrency: [0, 257, 1.5],
  queue_size: [0, 100001, 1.5],
  ttft_timeout_ms: [999, 60001, 1000.5],
  verdict_cache_days: [-1, 31, 0.5],
  hit_replay: ['bogus'],
  cache_entries: [999, 1000001, 1000.5],
  input_max_chars: [255, 100001, 256.5],
  input_max_chunks: [0, 33, 1.5],
  input_overflow: ['bogus'],
  input_max_images: [-1, 33, 0.5],
  max_inflight_mb: [15, 4097, 16.5],
  error_text_max_chars: [-1, 100001, 0.5],
  min_chars: [-1, 1001, 0.5]
}
const SWITCHES = ['auto_watch', 'record_pass', 'record_overturned', 'record_errors', 'store_text', 'record_pass_input_message']
const ENUMS = ['mode', 'fail_policy', 'hit_replay', 'input_overflow']
const DURATIONS = ['escalate_window_seconds', 'ban_window_seconds']
const SECRETS = ['notify_webhook_url', 'notify_webhook_secret']

type W = VueWrapper<any>
const row = (w: W, k: string) => w.findAllComponents(SSettingRow).find((r) => r.props('prop') === k)!
/** The control a user types into for key k. */
function control(w: W, k: string) {
  const r = row(w, k)
  if (SECRETS.includes(k)) return r.findComponent(SSecretInput).findComponent(SInput)
  if (SWITCHES.includes(k)) return r.findComponent(SSwitch)
  if (ENUMS.includes(k)) return r.findComponent(SSegmented)
  if (DURATIONS.includes(k)) return r.findComponent(DurationInput)
  return r.findComponent(SInput)
}
type Edit = (w: W) => unknown
const typeIn = (k: string, v: unknown): Edit => (w) => control(w, k).vm.$emit('update:modelValue', v)

async function save(page: Page, ...edits: Edit[]) {
  const w = mount(SettingsTab, { props: { page }, attachTo: document.body }) as W
  await flushPromises()
  for (const e of edits) {
    await e(w)
    await flushPromises()
  }
  await w.find('form').trigger('submit')
  await flushPromises()
  const body = corePut.mock.calls.at(-1)?.[1] as { values: Record<string, unknown> } | undefined
  return { w, put: body?.values ?? null, errors: w.findAll('.input-error-text').map((e) => e.text()) }
}

describe('a settings page over settings of every key', () => {
  it.each(Object.keys(PAGES) as Page[])('shows the stored values on the %s page and saves nothing when nothing changed', async (page) => {
    stored = STORED
    const { w, put } = await save(page)
    for (const k of PAGES[page]) {
      const shown = (w.vm as any).$.setupState.values[k]
      expect(shown).toEqual(STORED[k])
    }
    expect(put).toBeNull()
    expect(toast).toHaveBeenCalledWith('settingsTab.noChanges', 'info')
  })

  it.each(Object.keys(PAGES) as Page[])('PUTs every stored value key by key, secrets as ******, with the one key changed on the %s page', async (page) => {
    for (const k of PAGES[page].filter((k) => k in LEGAL)) {
      stored = STORED
      corePut.mockClear()
      const { put } = await save(page, typeIn(k, LEGAL[k]))
      expect(put).toEqual({ ...STORED, [k]: LEGAL[k] })
    }
  })

  it('drops the stored 0.1 API key, and only it, on any save', async () => {
    for (const page of Object.keys(PAGES) as Page[]) {
      stored = { ...STORED, api_key: MASK, base_url: 'https://old.example' }
      corePut.mockClear()
      expect((await save(page)).put).toEqual(STORED)
    }
  })

  it('removes a number left empty, so its default applies', async () => {
    stored = STORED
    const { put } = await save('records', typeIn('hit_retention_days', null))
    const { hit_retention_days: _, ...rest } = STORED
    expect(put).toEqual(rest)
  })
})

describe('a settings page over nothing stored', () => {
  it('shows the defaults and saves only the key changed', async () => {
    for (const page of Object.keys(PAGES) as Page[]) {
      for (const k of PAGES[page].filter((k) => k in LEGAL)) {
        stored = {}
        corePut.mockClear()
        const { w, put } = await save(page, typeIn(k, LEGAL[k]))
        expect(put).toEqual({ [k]: LEGAL[k] })
        w.unmount()
      }
      stored = {}
      const { w } = await save(page)
      for (const k of PAGES[page]) expect((w.vm as any).$.setupState.values[k]).toEqual(DEFAULTS[k as keyof typeof DEFAULTS])
    }
  })
})

describe('the rules', () => {
  it('refuse every value the settings form refused, at its row, without a request', async () => {
    for (const page of Object.keys(PAGES) as Page[]) {
      for (const k of PAGES[page].filter((k) => k in ILLEGAL)) {
        for (const v of ILLEGAL[k]) {
          stored = STORED
          corePut.mockClear()
          const { w, put } = await save(page, typeIn(k, v))
          expect(put, `${k} = ${v}`).toBeNull()
          expect(row(w, k).find('.input-error-text').exists(), `${k} = ${v}`).toBe(true)
          w.unmount()
        }
      }
    }
  })
})

describe('the webhook address and secret', () => {
  it.each(SECRETS)('keep %s while its box is left empty, take a new value, and clear it with its button', async (k) => {
    stored = STORED
    expect((await save('advanced', typeIn(k, 'typed'), typeIn(k, ''))).put).toBeNull()
    stored = STORED
    const typed = k === 'notify_webhook_url' ? ' https://hooks.example/x ' : 'new-secret'
    expect((await save('advanced', typeIn(k, typed))).put).toEqual({ ...STORED, [k]: typed.trim() })
    stored = STORED
    const { put } = await save('advanced', (w) => row(w, k).find('[data-testid="secret-clear"]').trigger('click'))
    const { [k]: _, ...rest } = STORED
    expect(put).toEqual(rest)
  })

  it('types the address in a text box and the secret in a password box', async () => {
    stored = STORED
    const w = mount(SettingsTab, { props: { page: 'advanced' } })
    await flushPromises()
    expect(row(w, 'notify_webhook_url').find('input').attributes('type')).toBe('text')
    expect(row(w, 'notify_webhook_secret').find('input').attributes('type')).toBe('password')
  })

  it('asks to re-enter a kept secret the core will not keep', async () => {
    stored = STORED
    corePut.mockRejectedValueOnce(new ApiError(400, { code: 'invalid_argument', message: 'x', details: { fields: [{ field: 'values/notify_webhook_secret', code: 'reenter', message: 'x' }] } }))
    const { w } = await save('advanced', typeIn('error_rate_alert_percent', 50))
    expect(row(w, 'notify_webhook_secret').find('.input-error-text').text()).toBe('settingsTab.reenter')
  })
})

describe('the Jev default set', () => {
  const jevTable = (table: unknown): Edit => (w) => w.findComponent(JevQuestionsEditor).vm.$emit('update:modelValue', structuredClone(table))
  it('copies the built-in set, sends thresholds typed as text as numbers, and goes back to the built-in one', async () => {
    stored = STORED
    const custom: Edit = (w) => w.findAllComponents(SSegmented)[0].vm.$emit('update:modelValue', 'builtin')
    expect((await save('jev', custom)).put).toEqual({ ...STORED, jev_default_set: null })
    stored = { ...STORED, jev_default_set: null }
    const copy: Edit = (w) => w.findAllComponents(SSegmented)[0].vm.$emit('update:modelValue', 'custom')
    expect((await save('jev', copy)).put).toEqual({ ...STORED, jev_default_set: BUILTIN })
    stored = STORED
    expect((await save('jev', jevTable({ preamble: 'x', items: [{ ...ROW, threshold: '0.7' }] }))).put).toEqual({ ...STORED, jev_default_set: { preamble: 'x', items: [{ ...ROW, threshold: 0.7 }] } })
  })

  it('refuses the tables the settings form refused', async () => {
    const bad = [
      { preamble: '', items: [] },
      { preamble: '', items: Array(33).fill(ROW) },
      { preamble: 'x'.repeat(2001), items: [ROW] },
      { preamble: '', items: [{ ...ROW, category: 'Bad' }] },
      { preamble: '', items: [{ ...ROW, label: 'x'.repeat(65) }] },
      { preamble: '', items: [{ ...ROW, question: '' }] },
      { preamble: '', items: [{ ...ROW, answer_yes: 'x'.repeat(1001) }] },
      { preamble: '', items: [{ ...ROW, threshold: 0 }] },
      { preamble: '', items: [{ ...ROW, threshold: 1.5 }] }
    ]
    for (const table of bad) {
      stored = STORED
      corePut.mockClear()
      const { w, put } = await save('jev', jevTable(table))
      expect(put, JSON.stringify(table).slice(0, 80)).toBeNull()
      expect(row(w, 'jev_default_set').find('.input-error-text').exists()).toBe(true)
      w.unmount()
    }
  })
})

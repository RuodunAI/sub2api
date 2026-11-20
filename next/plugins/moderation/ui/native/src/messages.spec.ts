/// <reference types="vite/client" />
import { describe, expect, it } from 'vitest'
import { checkMessages } from '@sub2api/ui/testing'
import messages from './messages'
import { validateSpec } from './flowValidate'
import type { Spec } from './types'

// Groups of keys looked up with a computed suffix: enumLabel / enumOptions
// (format.ts) take the group, the rest are t(`group.${…}`) in the views.
const ENUM_GROUPS = [
  'auditorType', 'auditors.preset', 'channelType', 'combine', 'cost.range', 'costSource', 'deniedAction', 'detail.level', 'detail.match', 'err',
  'fallbackReason', 'flows.combineOpt', 'flows.scope', 'flows.simUser', 'level', 'modelFilter', 'priceReason', 'protocol', 'result', 'review',
  'role', 'settingsTab.format', 'severity', 'source', 'stageResult', 'stats.granularity', 'timing', 'trend', 'userStatus', 'watchAction',
  'watchList.lockedOpt', 'watchList.sort', 'watchSource'
]
const COMPUTED = [
  'auditors.defaultState', 'auditors.diff', 'auditors.restoreConfirm', 'auditors.viewDefault', 'bindings.kind', 'bindings.kindHint', 'cat',
  'channelTypeHint', 'detail.f', 'duration', 'flows.coverage', 'flows.issue', 'flows.role', 'flows.simUserHint', 'flows.template', 'jev_default_set.items',
  'kpi', 'lists.hint', 'lists.name', 'lock.action', 'lock.confirm', 'mode', 'nav.desc', 'nav.items', 'perf.tile', 'queue.name', 'scoreLevel',
  'settingsTab.field', 'settingsTab.option', 'settingsTab.section', 'settingsTab.unit', 'statusLine', 'test.sample', 'todayCost', 'trace', 'userDaily',
  'verdict', 'watchActions.add', 'watchActions.desc', 'watchActions.set', 'watchActions.title', 'watchEdit.defaultExpiry'
]

describe('messages', () => {
  checkMessages(messages, {
    sources: import.meta.glob<string>(['./**/*.vue', './**/*.ts', '!./**/*.spec.ts', '!./messages.ts'], { query: '?raw', import: 'default', eager: true }),
    // flowOps passOutcome / hitOutcome and FlowEditor saveLive build the rest: t(`flows.${…}`), t(`flows.${field}Saved`).
    dynamic: [...[...ENUM_GROUPS, ...COMPUTED].map((g) => `${g}.`), 'flows.pass', 'flows.hit', 'flows.enabledSaved', 'flows.scopeSaved']
  })

  // 0.8.0 (new-api parity): the names the views build keys from.
  it('has a text for every auditor type, stage role, stage state and default level', () => {
    for (const lang of [messages.zh, messages.en]) {
      expect(['openai_moderation', 'typesafe', 'llm_verdict', 'llm_questions'].filter((k) => !(k in lang.auditorType))).toEqual([])
      expect(['first', 'review', 'fallback'].filter((k) => !(k in lang.flows.role))).toEqual([])
      expect(['hit', 'pass', 'skipped', 'error', 'failed'].filter((k) => !(k in lang.stageResult))).toEqual([])
      for (const group of [lang.mode, lang.statusLine.levelHint]) expect(Object.keys(group).sort()).toEqual(['enforce', 'observe', 'off'])
      expect(Object.keys(lang.fallbackReason).sort()).toEqual(['deleted', 'disabled', 'invalid', 'unpublished'])
      expect(Object.keys(lang.flows.scope).sort()).toEqual(['all', 'input', 'tools'])
    }
  })

  it('has a text for the issues of a flow with misplaced fallback stages', () => {
    const bad: Spec = { model_filter: { type: 'all', models: [] }, sample_rate: 100, total_timeout_ms: 0, stages: [{ auditors: [1], combine: 'any', fallback: true }, { auditors: [1], combine: 'any' }] }
    const codes = validateSpec(bad, () => 'openai_moderation').map((i) => i.code)
    expect(codes).toEqual(['fallback_first', 'fallback_order'])
    for (const lang of [messages.zh, messages.en]) expect(codes.filter((c) => !(c in lang.flows.issue))).toEqual([])
  })

  it('no longer says that an errored stage passes, or that exempt users are left unmoderated', () => {
    const all = (o: unknown): string[] => (o && typeof o === 'object' ? Object.values(o).flatMap(all) : [String(o)])
    const zh = all(messages.zh).join('\n')
    const en = all(messages.en).join('\n')
    for (const old of ['按通过处理', '出错一律放行', '出错放行', '本级按通过', '所在级放行', '本级放行', '免审用户']) expect(zh).not.toContain(old)
    for (const old of ['treated as a pass', 'Errors always allow', 'errors allow', 'this stage passes', 'its stage allows', 'the stage allows', 'exempt users']) expect(en).not.toContain(old)
  })

  it('names the fallback arrow and the cache counter as 0.8.0 behaves', () => {
    // a later review stage that passes overturns the hit and ends the run: "pass → fallback" is wrong
    expect(messages.zh.flows.toFallback).not.toContain('通过')
    expect(messages.en.flows.toFallback.toLowerCase()).not.toContain('pass')
    // hits are cached too (replayed); cache_hits counts requests passed from the cache
    expect(messages.zh.runtime.cacheHits).not.toContain('通过缓存')
    expect(messages.en.runtime.cacheHits.toLowerCase()).not.toContain('pass cache')
    for (const lang of [messages.zh, messages.en]) expect(['passAllow', 'passFallback', 'passOverturn'].filter((k) => !(k in lang.flows))).toEqual([])
  })

  // overview/QueueCard.vue: the runtime rows, kv_writing and kv_skipped among
  // them (0.8.1). Exactly these, so a key no row uses does not come back.
  it('has the texts of the runtime rows of the overview, and no others', () => {
    const rows = ['flows', 'inflight', 'calls', 'errorsN', 'cacheHits', 'cacheBytes', 'kvErrors', 'kvWriting', 'kvSkipped', 'imagesRefused', 'avgLatency']
    for (const lang of [messages.zh, messages.en]) expect(Object.keys(lang.runtime).sort()).toEqual([...rows].sort())
  })
})

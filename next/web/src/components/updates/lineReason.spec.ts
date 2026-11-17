import { describe, expect, it } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import type { LineCheck } from '@/stores/updates'
import { useLineReason } from './lineReason'

function reasonOf(check: Partial<LineCheck>, locale = 'zh') {
  i18n.global.locale.value = locale as typeof i18n.global.locale.value
  let text = ''
  mount(defineComponent({ setup() { text = useLineReason()({ has_update: false, compatible: false, ...check }); return () => h('div') } }), { global: { plugins: [i18n] } })
  return text
}

describe('useLineReason', () => {
  it('words coded reasons in the console language', () => {
    expect(reasonOf({ reason_code: 'no_release', reason: 'the update source has no stable core release' })).toContain('更新源还没有这条线的稳定版本')
    expect(reasonOf({ reason_code: 'version_conflict', reason: 'same version' })).toContain('签名身份不同')
    expect(reasonOf({ reason_code: 'no_release' }, 'en')).toContain('no stable release of this line')
  })

  it('keeps the findings of an incompatible release after a localized lead', () => {
    expect(reasonOf({ reason_code: 'incompatible', reason: 'peer protocol 3, cluster 2' })).toBe('此版本与当前集群不兼容：peer protocol 3, cluster 2')
    expect(reasonOf({ reason_code: 'incompatible' })).toBe('此版本与当前集群不兼容')
  })

  it('passes an uncoded reason through and says nothing without one', () => {
    expect(reasonOf({ reason: 'something else' })).toBe('something else')
    expect(reasonOf({})).toBe('')
  })
})

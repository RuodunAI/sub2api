import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { i18n } from '@/i18n'
import { useIssueText } from './upgrades'

function issueText(locale: 'zh' | 'en') {
  i18n.global.locale.value = locale
  let text!: ReturnType<typeof useIssueText>
  mount({ setup: () => { text = useIssueText(); return () => null } }, { global: { plugins: [i18n] } })
  return text
}

describe('useIssueText', () => {
  it('words a code with its params, and with the message when only it names the detail', () => {
    const zh = issueText('zh')
    expect(zh({ code: 'node_not_ready', params: { node: 'n2' }, message: 'node not locally ready: n2' })).toBe('节点 n2 没有在本地提供服务')
    expect(zh({ code: 'too_few_serving', params: { min: '2' }, message: 'fewer than 2 nodes serve' })).toContain('少于 2 个')
    expect(zh({ code: 'single_node', params: { seconds: '10' }, message: 'single node: about 10 s interruption' })).toContain('约 10 秒')
    expect(zh({ code: 'plugin_incompatible', message: 'plugin guard 1.2.0 requires core <0.1.300' })).toBe('有已启用的插件不支持目标核心：plugin guard 1.2.0 requires core <0.1.300')
    expect(issueText('en')({ code: 'node_off_baseline', params: { node: 'n3' }, message: 'node not on approved baseline: n3' })).toBe('Node n3 does not run the approved baseline')
  })

  it('shows the message of a code it does not know', () => {
    expect(issueText('zh')({ code: 'brand_new', params: { node: 'n1' }, message: 'something new about n1' })).toBe('something new about n1')
  })
})

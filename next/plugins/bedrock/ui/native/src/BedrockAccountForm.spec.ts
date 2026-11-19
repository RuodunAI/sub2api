import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import BedrockAccountForm from './BedrockAccountForm.vue'
import { setHost } from './host'
import { CASES, ORDER } from './form.cases'
import { editor, fakeHost, fill, saved } from './form.testkit'

beforeEach(() => setHost(fakeHost()))
afterEach(() => {
  setHost(null)
  document.body.innerHTML = ''
})

describe('BedrockAccountForm: v-model and verdict of every case', () => {
  for (const c of CASES) {
    it(c.name, async () => {
      const { w, credentials, validate } = editor(BedrockAccountForm, { type: c.type, mode: c.mode, modelValue: c.stored ? { ...c.stored } : undefined })
      await flushPromises()
      await fill(w, ORDER[c.type], c.input ?? {})
      expect(saved(credentials.value)).toEqual(c.out)
      expect(await validate()).toBe(c.ok)
      w.unmount()
    })
  }
})

describe('BedrockAccountForm: the fields of each type', () => {
  const labels = (type: string) => {
    const { w } = editor(BedrockAccountForm, { type })
    return w.findAll('label').map((l) => l.text())
  }

  it('shows the credentials of the type, then the shared region section', () => {
    expect(labels('bedrock_apikey')).toEqual(['API key*', 'Region', 'Cross-region inference prefix'])
    expect(labels('bedrock_aksk')).toEqual(['Access key ID*', 'Secret access key*', 'Region', 'Cross-region inference prefix'])
    expect(labels('bedrock_sts')).toEqual(['Access key ID*', 'Secret access key*', 'Session token*', 'Credentials expire at (optional)', 'Region', 'Cross-region inference prefix'])
  })

  it('keys and the session token are secret boxes; the access key ID is plain text', () => {
    const { w } = editor(BedrockAccountForm, { type: 'bedrock_sts' })
    expect(w.findAll('input').map((i) => i.attributes('type'))).toEqual(['text', 'password', 'password', 'datetime-local', 'text'])
    expect(w.findAll('input')[0].attributes('placeholder')).toBe('ASIA...')
    expect(editor(BedrockAccountForm, { type: 'bedrock_aksk' }).w.find('input').attributes('placeholder')).toBe('AKIA...')
  })

  it('names the region section with its note, and the prefix choices', () => {
    const { w } = editor(BedrockAccountForm, { type: 'bedrock_aksk' })
    expect(w.find('h4').text()).toBe('Region and models')
    expect(w.text()).toContain('Claude requests go to InvokeModel.')
    expect(w.findAll('option').map((o) => o.text())).toEqual(expect.arrayContaining(['None', 'us', 'eu', 'apac', 'jp', 'au', 'in', 'global']))
  })

  it('a stored secret shows as kept and goes back masked', async () => {
    const stored = { access_key_id: 'AKIAIOSFODNN7EXAMPLE', secret_access_key: '******', session_token: '******', region: 'us-east-1', inference_profile_prefix: 'global' }
    const { w, credentials, validate } = editor(BedrockAccountForm, { type: 'bedrock_sts', mode: 'edit', modelValue: { ...stored } })
    await flushPromises()
    expect(w.findAll('input[type="password"]').map((i) => i.attributes('placeholder'))).toEqual(['******', '******'])
    expect(await validate()).toBe(true)
    expect(credentials.value).toEqual(stored)
  })

  it('shows the server errors at their fields until the field changes', async () => {
    const { w } = editor(BedrockAccountForm, { type: 'bedrock_aksk', errors: { region: 'the AWS China regions (cn-*) are not supported' } })
    await flushPromises()
    expect(w.text()).toContain('the AWS China regions')
    await fill(w, ORDER.bedrock_aksk, { region: 'us-west-2' })
    expect(w.text()).not.toContain('the AWS China regions')
  })
})
